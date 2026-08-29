package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/authevent"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxPendingMCPAuthCorrelations = 8

// pendingMCPAuthCorrelation retains only the live process state needed to route one committed browser grant.
type pendingMCPAuthCorrelation struct {
	session   *mcpSession
	appID     uuid.UUID
	expiresAt time.Time
}

// mcpAuthCorrelationRegistry provides exact lookup and bounded reverse cleanup for process-local MCP sessions.
type mcpAuthCorrelationRegistry struct {
	sync.Mutex
	byConnectSession map[uuid.UUID]pendingMCPAuthCorrelation
	byMCPSession     map[string]map[uuid.UUID]time.Time
}

var pendingMCPAuthCorrelations = mcpAuthCorrelationRegistry{
	byConnectSession: make(map[uuid.UUID]pendingMCPAuthCorrelation),
	byMCPSession:     make(map[string]map[uuid.UUID]time.Time),
}

// reserveMCPAuthAction claims capacity before Engine persists an OAuth session that must remain deliverable to this MCP client.
func reserveMCPAuthAction(ctx context.Context, session *mcpSession) bool {
	// Only fully initialized Streamable sessions carry the semaphore used to bind persistence and notification capacity.
	if session == nil || session.authActions == nil {
		recordMCPAuthCorrelation(ctx, "capacity_unavailable")
		return false
	}
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	// Lifecycle ownership prevents a terminal session from accepting new durable browser state during teardown.
	if !mcpSessionRegisteredAndActiveLocked(session) {
		recordMCPAuthCorrelation(ctx, "session_unavailable")
		return false
	}
	pruned := pendingMCPAuthCorrelations.pruneSession(session.sessionID, time.Now().UTC())
	releaseMCPAuthActions(session, pruned)
	// A non-blocking claim rejects excess work before the canonical connect mutation inserts its database row.
	select {
	case session.authActions <- struct{}{}:
		recordMCPAuthCorrelation(ctx, "reserved")
		return true
	default:
		recordMCPAuthCorrelation(ctx, "capacity")
		return false
	}
}

// releaseMCPAuthAction returns one reservation after failure, expiry, teardown, or notification dequeue.
func releaseMCPAuthAction(session *mcpSession) {
	releaseMCPAuthActions(session, 1)
}

// releaseMCPAuthActions returns bounded reservations without blocking cleanup when synthetic tests did not allocate the semaphore.
func releaseMCPAuthActions(session *mcpSession, count int) {
	// Missing state or a non-positive count means no production reservation can be owned here.
	if session == nil || session.authActions == nil || count <= 0 {
		return
	}
	for index := 0; index < count; index++ {
		// Cleanup remains defensive so an invariant breach cannot deadlock session termination.
		select {
		case <-session.authActions:
		default:
			return
		}
	}
}

// StartMCPAuthCompletionSubscriber fans the internal lifecycle stream to every replica because only one process owns the addressed MCP session.
func StartMCPAuthCompletionSubscriber(ctx context.Context, client *messaging.NATSClient) error {
	// A core subscription is intentionally ephemeral: process restart also terminates every process-local MCP session it could notify.
	if client == nil || client.Conn == nil {
		return errors.New("MCP auth completion subscriber requires NATS")
	}
	subscription, err := client.Conn.Subscribe(messaging.EngineAuthEventsSubject, func(message *nats.Msg) {
		handleMCPAuthLifecycleEvent(context.Background(), message.Data)
	})
	// Startup must fail before serving MCP traffic when completion events cannot reach this replica.
	if err != nil {
		return err
	}
	// Flush proves the replica is listening before a newly issued browser session can complete.
	if err := client.Conn.Flush(); err != nil {
		_ = subscription.Unsubscribe()
		return err
	}
	context.AfterFunc(ctx, func() { _ = subscription.Unsubscribe() })
	return nil
}

// registerMCPAuthCorrelation binds one persisted connect UUID before its browser URL is returned to the client.
func registerMCPAuthCorrelation(ctx context.Context, session *mcpSession, elicitationID, expiresAt string) bool {
	connectSessionID, idErr := uuid.Parse(elicitationID)
	expiry, expiryErr := time.Parse(time.RFC3339, expiresAt)
	// Browser actions without exact live identity or a future absolute expiry cannot become routable process state.
	if session == nil || connectSessionID == uuid.Nil || idErr != nil || expiryErr != nil || !expiry.After(time.Now().UTC()) {
		recordMCPAuthCorrelation(ctx, "invalid")
		return false
	}
	appID, appErr := uuid.Parse(session.appID)
	// App provenance is checked again when the durable event arrives, so an unparseable owner cannot be admitted now.
	if appErr != nil || appID == uuid.Nil {
		recordMCPAuthCorrelation(ctx, "invalid_app")
		return false
	}
	// Lifecycle ownership orders the active-state check and registry insert before termination removes and cleans the session.
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	// A terminating or replaced session must not retain a correlation that can target a different runtime later.
	if !mcpSessionRegisteredAndActiveLocked(session) {
		recordMCPAuthCorrelation(ctx, "session_unavailable")
		return false
	}
	outcome := pendingMCPAuthCorrelations.register(session, appID, connectSessionID, expiry, time.Now().UTC())
	recordMCPAuthCorrelation(ctx, outcome)
	return outcome == "registered"
}

// register prunes expired entries and admits one exact correlation without exceeding the per-session ceiling.
func (registry *mcpAuthCorrelationRegistry) register(session *mcpSession, appID, connectSessionID uuid.UUID, expiresAt, now time.Time) string {
	registry.Lock()
	pruned := registry.pruneSessionLocked(session.sessionID, now)
	defer func() {
		registry.Unlock()
		// Expired correlations release their existing reservations only after registry ownership ends.
		releaseMCPAuthActions(session, pruned)
	}()
	pending := registry.byMCPSession[session.sessionID]
	// Reusing a canonical connect UUID is rejected because this attempt already owns a distinct pre-persistence reservation.
	if existing, ok := registry.byConnectSession[connectSessionID]; ok {
		if existing.session == session && existing.appID == appID && existing.expiresAt.Equal(expiresAt) {
			return "duplicate"
		}
		return "collision"
	}
	// The ceiling prevents a busy client from retaining unbounded browser sessions while earlier prompts remain unresolved.
	if len(pending) >= maxPendingMCPAuthCorrelations {
		return "capacity"
	}
	// The reverse index makes session teardown proportional only to that session's bounded pending actions.
	if pending == nil {
		pending = make(map[uuid.UUID]time.Time, maxPendingMCPAuthCorrelations)
		registry.byMCPSession[session.sessionID] = pending
	}
	pending[connectSessionID] = expiresAt
	registry.byConnectSession[connectSessionID] = pendingMCPAuthCorrelation{session: session, appID: appID, expiresAt: expiresAt}
	return "registered"
}

// pruneSession removes expired correlations under registry ownership and reports how many reservations can be returned.
func (registry *mcpAuthCorrelationRegistry) pruneSession(sessionID string, now time.Time) int {
	registry.Lock()
	defer registry.Unlock()
	return registry.pruneSessionLocked(sessionID, now)
}

// pruneSessionLocked removes only expired actions from one bounded reverse index while the caller owns the registry lock.
func (registry *mcpAuthCorrelationRegistry) pruneSessionLocked(sessionID string, now time.Time) int {
	pending := registry.byMCPSession[sessionID]
	pruned := 0
	// Each session holds at most eight entries, so pruning work is fixed independently of workspace size.
	for connectSessionID, expiresAt := range pending {
		if now.Before(expiresAt) {
			continue
		}
		delete(pending, connectSessionID)
		delete(registry.byConnectSession, connectSessionID)
		pruned++
	}
	// Empty reverse indexes are removed so short-lived sessions leave no retained keys.
	if len(pending) == 0 {
		delete(registry.byMCPSession, sessionID)
	}
	return pruned
}

// unregisterMCPAuthCorrelations removes every pending browser action when its owning MCP session terminates.
func unregisterMCPAuthCorrelations(session *mcpSession) {
	// Synthetic or partially started sessions may never have acquired a correlation.
	if session == nil {
		return
	}
	pendingMCPAuthCorrelations.Lock()
	pending := pendingMCPAuthCorrelations.byMCPSession[session.sessionID]
	for connectSessionID := range pending {
		delete(pendingMCPAuthCorrelations.byConnectSession, connectSessionID)
	}
	delete(pendingMCPAuthCorrelations.byMCPSession, session.sessionID)
	pendingMCPAuthCorrelations.Unlock()
	// Pending entries still own reservations; already-queued notifications belong to the retiring session and need no future admission.
	releaseMCPAuthActions(session, len(pending))
}

// handleMCPAuthLifecycleEvent admits one durable connected-auth event before attempting process-local delivery.
func handleMCPAuthLifecycleEvent(ctx context.Context, payload []byte) {
	event, err := authevent.Decode(payload)
	// Malformed durable data cannot address runtime state and remains observable as one bounded outcome.
	if err != nil {
		recordMCPAuthCompletion(ctx, "invalid_event", true)
		return
	}
	// Token refresh transitions have no browser elicitation and are consumed by their existing SDK projector only.
	if event.Type != authevent.TypeConnectionCompleted {
		return
	}
	outcome := pendingMCPAuthCorrelations.complete(event, time.Now().UTC())
	// Unmatched fanout is expected on every non-owning replica and for non-MCP connections, so it creates no trace volume.
	if outcome != "unmatched" {
		recordMCPAuthCompletion(ctx, outcome, outcome != "delivered")
	}
}

// complete consumes an exact correlation only after app provenance and notification admission succeed.
func (registry *mcpAuthCorrelationRegistry) complete(event authevent.Event, now time.Time) string {
	registry.Lock()
	// Decode guarantees completion identity, but the local registry still treats absent correlation as an unrelated app or replica.
	if event.ConnectSessionID == nil {
		registry.Unlock()
		return "invalid_event"
	}
	pending, ok := registry.byConnectSession[*event.ConnectSessionID]
	// Every replica sees the event; only the process holding this exact UUID performs delivery.
	if !ok {
		registry.Unlock()
		return "unmatched"
	}
	// Expired browser state cannot wake a client after the advertised action lifetime.
	if !now.Before(pending.expiresAt) {
		registry.removeLocked(pending.session.sessionID, *event.ConnectSessionID)
		registry.Unlock()
		releaseMCPAuthAction(pending.session)
		return "expired"
	}
	// App provenance prevents a malformed or incorrectly attributed lifecycle event from crossing MCP family ownership or consuming the later valid completion.
	if event.CreatedByAppID == nil || *event.CreatedByAppID != pending.appID {
		registry.Unlock()
		return "identity_mismatch"
	}
	// Taking the exact correlation before releasing the registry lock makes duplicate lifecycle deliveries idempotent.
	registry.removeLocked(pending.session.sessionID, *event.ConnectSessionID)
	registry.Unlock()
	// Queue admission is non-blocking; the Streamable GET owns eventual standard notification delivery.
	if !enqueueMCPAuthCompletion(pending.session, event.ConnectSessionID.String()) {
		// Failed queue admission ends this browser action because no later durable event can match the consumed correlation.
		releaseMCPAuthAction(pending.session)
		return "queue_unavailable"
	}
	return "delivered"
}

// removeLocked deletes both lookup directions while the caller owns the registry lock.
func (registry *mcpAuthCorrelationRegistry) removeLocked(sessionID string, connectSessionID uuid.UUID) {
	delete(registry.byConnectSession, connectSessionID)
	pending := registry.byMCPSession[sessionID]
	delete(pending, connectSessionID)
	// Removing empty reverse state keeps completed sessions absent from future bounded scans.
	if len(pending) == 0 {
		delete(registry.byMCPSession, sessionID)
	}
}

// recordMCPAuthCorrelation attaches one bounded registration decision to the existing agent-triggered start span.
func recordMCPAuthCorrelation(ctx context.Context, outcome string) {
	trace.SpanFromContext(ctx).AddEvent("engine.mcp.auth_correlation.register", trace.WithAttributes(
		attribute.String("auth.correlation.outcome", outcome),
	))
}

// recordMCPAuthCompletion records cross-replica notification routing without retaining session, app, user, or provider identifiers.
func recordMCPAuthCompletion(ctx context.Context, outcome string, failed bool) {
	_, span := otel.Tracer("engine").Start(ctx, "engine.sandbox.mcp.auth_completion")
	defer span.End()
	span.SetAttributes(attribute.String("auth.completion.outcome", outcome))
	// Invalid or undeliverable addressed events are errors; ordinary unmatched replica fanout is expected.
	if failed {
		span.SetStatus(codes.Error, "MCP auth completion delivery failed")
	}
}
