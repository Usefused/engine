package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/runtime"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxMCPMessageBodyBytes   = 256 * 1024
	maxMCPSessionIdleTimeout = 5 * time.Minute
	maxMCPSSEFailureQueue    = 4
	mcpSSEFailureDrainGrace  = 250 * time.Millisecond
	// A one-MiB result can double when nested as JSON text; this also reserves
	// room for the bounded request ID and the fixed JSON-RPC envelope.
	maxMCPResponseMessageBytes = 3 * 1024 * 1024
)

// mcpSseHandler handles SSE connections for MCP sessions.
//
// Isolation guarantee: each connection spawns a brand-new `node` process.
// No memory, globals, or file handles are shared between sessions.
func mcpSseHandler(w http.ResponseWriter, r *http.Request) {
	// Origin admission precedes bearer parsing so browser cross-origin traffic cannot probe credentials.
	if !admitMCPRequestOrigin(w, r) {
		return
	}
	routeID, token, ok := extractMCPParams(w, r)
	// Missing runtime identity must fail before any session or provenance is retained.
	if !ok {
		return
	}
	target, err := resolveMCPRoute(r.Context(), routeID)
	// Resolution failures remain indistinguishable from invalid runtime
	// credentials so an anonymous caller cannot enumerate MCP families.
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	appIDHex := target.AppID.String()
	authContext, err := mcpSessionAuthContext(r.Header)
	// Invalid connection selectors cannot become authority for a new session.
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Rate limiting applies before allocating a provider-capable runtime.
	if !allowMCPSessionStart(w, appIDHex) {
		return
	}

	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sandbox.mcp.concurrency_check")
	// Shared concurrency policy must admit the connection before an isolated process starts.
	if limitErr := entitlement.CheckLimit(span, "mcp_sandbox_concurrency", activeMCPSessionCount(), entitlement.LiveEntitlement.Load().MaxSandboxConcurrency); limitErr != nil {
		slog.InfoContext(ctx, "mcp sse denied: max sandbox concurrency reached", "limit", limitErr.Limit, "current", limitErr.Current)
		writeError(w, http.StatusPaymentRequired, limitErr.Error())
		span.End()
		return
	}
	span.End()

	identity, connected := connectMCPApp(w, r.Context(), appIDHex, token)
	// An unauthenticated peer must not create a durable session attribution.
	if !connected {
		return
	}
	sessionID, sessionCtx, stdout, started := startMCPSSESession(r, w, routeID, appIDHex, token, authContext, identity)
	// Failed runtime preparation already returned one bounded handshake error.
	if !started {
		return
	}
	defer terminateMCPSession(sessionID, "client_disconnected")

	flusher, ok := setupSSEResponse(w, sessionID)
	// A writer without streaming support cannot keep the newly created session alive.
	if !ok {
		return
	}
	processMCPStream(sessionCtx, w, flusher, stdout, sessionID)
}

// startMCPSSESession prepares and registers one legacy runtime after shared admission succeeds.
func startMCPSSESession(r *http.Request, w http.ResponseWriter, routeID, appID, token string, authContext map[string]any, identity auth.RuntimeIdentity) (string, context.Context, io.ReadCloser, bool) {
	sessionID := uuid.NewString()
	sessionCtx, cancel := mcpSessionContext(r.Context(), identity.TokenPolicy.ExpiresAt)
	fixture, err := prepareSessionFixture(sessionCtx, appID, identity.TokenPolicy)
	// Typed schema admission failures remain actionable without leaking catalogue content.
	if err != nil {
		failMCPSSESessionStart(w, appID, sessionID, cancel, err)
		return "", nil, nil, false
	}
	cmd, err := buildMCPCommand(sessionCtx, sessionID, fixture)
	// Serialization admission uses the same public failure mapping as live fixture preparation.
	if err != nil {
		failMCPSSESessionStart(w, appID, sessionID, cancel, err)
		return "", nil, nil, false
	}
	stdin, stdout, err := setupPipesAndStart(cmd)
	// Failed process startup receives the generic runtime-start code rather than leaking process details.
	if err != nil {
		cleanupFailedMCPSessionStart(appID, sessionID, cancel)
		writeMCPSessionHTTPError(w, http.StatusInternalServerError, "MCP session could not start", mcpSessionStartFailureData(mcpSessionStartFailedCode))
		return "", nil, nil, false
	}
	registerMCPSession(sessionCtx, &mcpSession{
		clientMetadata: initialMCPSessionMetadata(r),
		appID:          appID, routeID: routeID, sessionID: sessionID, tokenID: identity.TokenID,
		protocolVersion: "2024-11-05", transport: "sse", cmd: cmd, stdin: stdin,
		cancel: cancel, token: token, fixture: fixture, authContext: authContext,
	})
	return sessionID, sessionCtx, stdout, true
}

// failMCPSSESessionStart maps catalogue preparation errors after releasing unregistered runtime ownership.
func failMCPSSESessionStart(w http.ResponseWriter, appID, sessionID string, cancel context.CancelFunc, err error) {
	cleanupFailedMCPSessionStart(appID, sessionID, cancel)
	statusCode, errorCode := mcpSessionStartFailure(err)
	writeMCPSessionHTTPError(w, statusCode, "MCP session could not start", mcpSessionStartFailureData(errorCode))
}

func connectMCPApp(w http.ResponseWriter, ctx context.Context, appID, token string) (auth.RuntimeIdentity, bool) {
	identity, err := validateMCPToken(ctx, appID, token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return auth.RuntimeIdentity{}, false
	}
	// The execution token authorizes this boundary only. Cache loading receives
	// no credential value because Registry access uses Engine identity.
	if err := globalObjectCache.ConnectSDK(ctx, appID); err != nil {
		writeMCPSessionHTTPError(w, http.StatusServiceUnavailable, "MCP session could not load its server configuration", mcpSessionStartFailureData(mcpSessionStartFailedCode))
		return auth.RuntimeIdentity{}, false
	}
	return identity, true
}

// extractMCPParams extracts and validates required URL parameters and headers.
func extractMCPParams(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	appIDHex := chi.URLParam(r, "id")
	if appIDHex == "" {
		writeError(w, http.StatusBadRequest, "app id required")
		return "", "", false
	}

	token, ok := extractBearerAuthToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "Authorization header required")
		return "", "", false
	}
	return appIDHex, token, true
}

// resolveMCPRoute converts either public MCP route identity into one exact,
// runnable version before token validation or session creation.
func resolveMCPRoute(ctx context.Context, routeIDHex string) (*store.MCPRouteTarget, error) {
	routeID, err := uuid.Parse(routeIDHex)
	// Malformed route identities cannot reach persistence or token validation.
	if err != nil {
		return nil, err
	}
	// Missing route persistence is a startup wiring error; failing closed keeps
	// an SDK Version ID from being accepted accidentally as an MCP runtime.
	if globalMCPRouteResolver == nil {
		return nil, auth.ErrUnauthorized
	}
	return globalMCPRouteResolver.ResolveMCPRoute(ctx, routeID)
}

func validateMCPToken(ctx context.Context, appIDHex, token string) (auth.RuntimeIdentity, error) {
	if globalTokenValidator == nil {
		return auth.RuntimeIdentity{}, auth.ErrUnauthorized
	}
	appID, err := uuid.Parse(appIDHex)
	if err != nil {
		return auth.RuntimeIdentity{}, err
	}
	// Every session passes through the process-shared validator. Valid entries
	// are bounded to 30 seconds and precise revoke events evict them immediately.
	return globalTokenValidator.Validate(ctx, appID, token)
}

// mcpSessionContext preserves credential expiry without imposing an age limit on active work.
func mcpSessionContext(parent context.Context, expiresAt *time.Time) (context.Context, context.CancelFunc) {
	// Activity cannot extend authorization beyond the execution token's lifetime.
	if expiresAt != nil {
		return context.WithDeadline(parent, *expiresAt)
	}
	return context.WithCancel(parent)
}

func extractBearerAuthToken(authHeader string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	return strings.TrimSpace(token), ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != ""
}

// buildMCPCommand constructs the Node.js process for this MCP session.
//
// Security controls applied here (code-level, platform-independent):
//
//  1. Memory cap via NODE_OPTIONS (64 MB heap, 4 MB semi-space).
//  2. HOME and TMPDIR are redirected so the process cannot write to the
//     user home directory. TMPDIR is scoped to a per-session directory
//     that is cleaned up when the MCP session terminates.
//  3. applySysProcAttr sets Pdeathsig+Setpgid on Linux (no-op on macOS).
//
// The fixed runtime receives only a session identifier, Engine port, and
// scoped operation fixture. Provider credentials never enter the child
// process or its environment.
func buildMCPCommand(ctx context.Context, sessionID string, fixture *Fixture) (*exec.Cmd, error) {
	// Per-session storage keeps operation catalogs isolated between tenants.
	sessionTmpDir := mcpSessionTmpDir(sessionID)
	if err := os.MkdirAll(sessionTmpDir, 0700); err != nil {
		return nil, err
	}
	fixturePath, err := writeSessionFixture(sessionTmpDir, fixture)
	if err != nil {
		return nil, err
	}

	entrypoint := sharedRuntimeEntrypointPath()
	if entrypoint == "" {
		entrypoint = filepath.Join(sessionTmpDir, "bundle.js")
		if err := os.WriteFile(entrypoint, runtime.MCPSharedRuntimeBundle, 0600); err != nil {
			return nil, fmt.Errorf("write mcp bundle: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, "node", entrypoint)

	cmd.Env = []string{
		// Memory limits — keep the Node process lean.
		"NODE_OPTIONS=--max-old-space-size=64 --max-semi-space-size=4",
		"PATH=" + os.Getenv("PATH"),
		// FS isolation: redirect home and tmp writes to controlled locations.
		"HOME=/dev/null",
		"TMPDIR=" + sessionTmpDir,
		"FUSED_SESSION_ID=" + sessionID,
		"FUSED_ENGINE_PORT=" + globalEnginePort,
		"FUSED_FIXTURE_PATH=" + fixturePath,
	}

	cmd.Stderr = os.Stderr

	// Linux: kill child if parent dies; place child in own process group.
	applySysProcAttr(cmd)

	return cmd, nil
}

// sharedRuntimeEntrypointPath resolves the compiled shared MCP runtime's
// entrypoint, overridable via FUSED_SHARED_RUNTIME_PATH (mirrors
// sharedRuntimeFixturePath in sandbox.go for the same reason: deployments
// and tests don't all run with cwd == repo root).
func sharedRuntimeEntrypointPath() string {
	if p := os.Getenv("FUSED_SHARED_RUNTIME_PATH"); p != "" {
		return p
	}
	return ""
}

// prepareSessionFixture derives discovery from the exact authorized app scope.
// Keeping this outside the handler preserves one failure boundary for missing
// or incomplete immutable snapshots.
func prepareSessionFixture(ctx context.Context, appIDHex string, policy store.AppTokenPolicy) (*Fixture, error) {
	selections, err := validateAndParseScope(ctx, globalObjectCache, appIDHex)
	if err != nil {
		return nil, fmt.Errorf("load app scope: %w", err)
	}
	fixture, err := buildSessionFixture(ctx, globalObjectCache, appIDHex, selections, policy)
	// Source-schema admission can fail before a complete fixture exists and still needs bounded audit evidence.
	if err != nil {
		recordMCPSchemaLimit(ctx, appIDHex, err)
		return nil, fmt.Errorf("build fixture: %w", err)
	}
	// Live catalogue admission occurs before Node starts so unsafe schema work
	// cannot consume runtime memory or become model-visible documentation.
	if err := validateMCPFixtureSchemas(fixture); err != nil {
		recordMCPSchemaLimit(ctx, appIDHex, err)
		return nil, fmt.Errorf("admit fixture schemas: %w", err)
	}
	return fixture, nil
}

// writeSessionFixture serializes fixture to sessionTmpDir/fixture.json in the
// same operations plus optional public Unified descriptor shape fixture.ts
// parses, so the
// spawned Node process reads the exact catalog validated by the Go middleware.
// sessionTmpDir already exists
// (buildMCPCommand creates it) and is cleaned up by terminateMCPSession, so
// this file needs no cleanup of its own.
func writeSessionFixture(sessionTmpDir string, fixture *Fixture) (string, error) {
	// A nil fixture cannot produce a scoped catalogue and must never serialize
	// as JSON null for the child runtime to interpret permissively.
	if fixture == nil {
		return "", fmt.Errorf("no fixture available for this session")
	}
	// This serialization boundary remains a defense-in-depth check for callers
	// that construct fixtures without the live session preparation path.
	if err := validateMCPFixtureSchemas(fixture); err != nil {
		return "", fmt.Errorf("admit fixture schemas: %w", err)
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		return "", fmt.Errorf("marshal session fixture: %w", err)
	}
	path := filepath.Join(sessionTmpDir, "fixture.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", fmt.Errorf("write session fixture: %w", err)
	}
	return path, nil
}

func mcpSessionTmpDir(sessionID string) string {
	return filepath.Join(os.TempDir(), "fused-sandbox-"+sessionID)
}

// setupPipesAndStart initializes IO pipes and starts the Node process.
// A background goroutine calls cmd.Wait() to reap the zombie once it exits.
func setupPipesAndStart(cmd *exec.Cmd) (io.WriteCloser, io.ReadCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open stdin pipe")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open stdout pipe")
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start mcp server")
	}

	// Reap the process once it exits so it does not become a zombie.
	go func() { cmd.Wait() }()

	return stdin, stdout, nil
}

// registerMCPSession is shared by SSE and Streamable HTTP so expiry, idle
// termination, history events, and cleanup cannot drift by transport.
func registerMCPSession(ctx context.Context, sess *mcpSession) {
	sess.lifecycleCtx = ctx
	sess.pendingRequests = make(map[string]struct{})
	sess.searchTelemetry = make(map[string]*mcpSearchObservation)
	sess.pendingMayExecute = make(map[string]bool)
	// Legacy server failures need a bounded route to the already-open event stream.
	if sess.transport == "sse" && sess.sseFailures == nil {
		sess.sseFailures = make(chan mcpSSEFailure, maxMCPSSEFailureQueue)
	}
	touchMCPSession(sess)
	sess.idleTimer = time.AfterFunc(mcpSessionIdleTimeout(), func() { handleMCPSessionIdle(sess.sessionID) })
	// Lifecycle publication stays serialized with removal so an immediate revocation cannot overtake the started event.
	sess.lifecycleMu.Lock()
	mcpSessions.Lock()
	mcpSessions.m[sess.sessionID] = sess
	mcpSessions.Unlock()
	publishMCPSessionEventLocked(sess, "started", "")
	sess.lifecycleMu.Unlock()
	go terminateMCPSessionWhenContextEnds(ctx, sess.sessionID)
}

// mcpSessionIdleTimeout bounds inactivity only; the existing config key does not cap session age.
func mcpSessionIdleTimeout() time.Duration {
	configured := cfg.Sandbox.SessionMaxAgeSeconds
	// Invalid or excessive settings cannot disable cleanup of abandoned runtimes.
	if configured <= 0 || configured > int(maxMCPSessionIdleTimeout/time.Second) {
		return maxMCPSessionIdleTimeout
	}
	return time.Duration(configured) * time.Second
}

// handleMCPSessionIdle rechecks activity at the shared termination boundary because timers may race traffic.
func handleMCPSessionIdle(sessionID string) {
	terminateMCPSession(sessionID, "idle_timeout")
}

func terminateMCPSessionWhenContextEnds(ctx context.Context, sessionID string) {
	<-ctx.Done()
	reason := mcpSessionEndReason(ctx, sessionID)
	terminateMCPSession(sessionID, reason)
}

// mcpSessionEndReason keeps durable lifecycle outcomes stable without exposing context details.
func mcpSessionEndReason(ctx context.Context, sessionID string) string {
	reason := "engine_shutdown"
	// Legacy SSE is tied to its request context, so ordinary cancellation is a disconnect.
	if sess, ok := lookupMCPSession(sessionID); ok && sess.transport == "sse" {
		reason = "client_disconnected"
	}
	return canonicalMCPSessionEndReason(ctx, reason)
}

// canonicalMCPSessionEndReason makes the owned deadline authoritative even
// when child EOF or handler cleanup wins the race to remove the session.
func canonicalMCPSessionEndReason(ctx context.Context, fallback string) string {
	// Directly constructed test sessions retain the explicit cleanup outcome.
	if ctx == nil {
		return fallback
	}
	// Session deadlines now belong only to authentication or the parent request.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "token_expired"
	}
	return fallback
}

// mcpSessionAuthContext accepts the same stable connected-user selectors a
// generated SDK sends, but only during the authenticated MCP handshake. The
// model-facing search_docs/execute schemas therefore contain no auth inputs.
func mcpSessionAuthContext(headers http.Header) (map[string]any, error) {
	endUserRef := strings.TrimSpace(headers.Get("X-Fused-End-User-Ref"))
	resourceID := strings.TrimSpace(headers.Get("X-Fused-Resource-ID"))
	if len(endUserRef) > 255 {
		return nil, errors.New("X-Fused-End-User-Ref is too long")
	}
	if resourceID != "" {
		if _, err := uuid.Parse(resourceID); err != nil {
			return nil, errors.New("X-Fused-Resource-ID must be a valid UUID")
		}
	}
	context := map[string]any{}
	if endUserRef != "" {
		context["fused_end_user_ref"] = endUserRef
	}
	if resourceID != "" {
		context["fused_resource_id"] = resourceID
	}
	return context, nil
}

// terminateMCPSession deletes only the transport session. The execution token
// remains independently revocable/expirable, while history keeps the reason.
func terminateMCPSession(sessionID, reason string) bool {
	sess, ok := lookupMCPSession(sessionID)
	// A missing session has already been claimed by another lifecycle owner.
	if !ok {
		return false
	}
	// Removal and lifecycle publication share this lock with initialization so their durable order matches state order.
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	sess = claimMCPSessionTermination(sess, reason)
	// Concurrent or repeated termination must not duplicate lifecycle or search telemetry.
	if sess == nil {
		return false
	}
	reason = canonicalMCPSessionEndReason(sess.lifecycleCtx, reason)
	// Ending a session closes any in-flight search spans with one stable boundary code.
	finishMCPSearchSession(sess)
	// A live child process is best-effort killed after its in-flight observations close.
	if sess.cmd != nil && sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	}
	// Tests and failed starts may not have installed a cancellation function.
	if sess.cancel != nil {
		sess.cancel()
	}
	os.RemoveAll(mcpSessionTmpDir(sessionID))
	// Cache ownership exists only after the process-wide cache has initialized.
	if globalObjectCache != nil {
		globalObjectCache.DisconnectSDK(sess.appID)
	}
	// Only the stable idle-limit outcome enters OTEL; session IDs, tokens, and
	// catalogue or provider data remain exclusively outside telemetry.
	if reason == "idle_timeout" {
		recordMCPSessionIdleTimeout(sess)
	}
	publishMCPSessionEventLocked(sess, "ended", reason)
	return true
}

// claimMCPSessionTermination makes idle admission and session removal atomic with tracked work and activity.
func claimMCPSessionTermination(expected *mcpSession, reason string) *mcpSession {
	mcpSessions.Lock()
	defer mcpSessions.Unlock()
	sess, ok := mcpSessions.m[expected.sessionID]
	// Exact pointer matching prevents a delayed cleanup from claiming replacement state with the same opaque key.
	if !ok || sess != expected {
		return nil
	}
	sess.pendingMu.Lock()
	defer sess.pendingMu.Unlock()
	sess.activityMu.Lock()
	defer sess.activityMu.Unlock()
	// A stale timer cannot retire a session after a request or completion refreshed it.
	if reason == "idle_timeout" && deferMCPSessionIdleLocked(sess) {
		return nil
	}
	sess.ended = true
	// Stop under the activity lock so late request completion cannot rearm cleanup.
	if sess.idleTimer != nil {
		sess.idleTimer.Stop()
	}
	delete(mcpSessions.m, sess.sessionID)
	return sess
}

// deferMCPSessionIdleLocked grants pending work and recent traffic their full inactivity window.
// The caller holds pendingMu and activityMu through the eventual removal decision.
func deferMCPSessionIdleLocked(sess *mcpSession) bool {
	timeout := mcpSessionIdleTimeout()
	remaining := time.Until(sess.lastActivityAt.Add(timeout))
	// A long-running request is active even when no new client messages arrive.
	if len(sess.pendingRequests) > 0 {
		remaining = timeout
	}
	// Only genuine inactivity may end a session; server keepalives do not refresh activity.
	if remaining <= 0 {
		return false
	}
	// Directly constructed test sessions may not own a timer.
	if sess.idleTimer != nil {
		sess.idleTimer.Reset(remaining)
	}
	return true
}

// recordMCPSessionIdleTimeout audits abandoned-runtime cleanup without exposing session material.
func recordMCPSessionIdleTimeout(sess *mcpSession) {
	recordMCPLimitRejection(context.Background(), mcpLimitObservation{
		AppID: sess.appID, Transport: sess.transport,
		Kind: "session_idle", Unit: "milliseconds",
		Code: "mcp_session_idle_timeout", Maximum: mcpSessionIdleTimeout().Milliseconds(),
	})
}

// publishMCPSessionEvent serializes direct lifecycle publishers that do not already own the transition lock.
func publishMCPSessionEvent(sess *mcpSession, eventType, endReason string) {
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	publishMCPSessionEventLocked(sess, eventType, endReason)
}

// publishMCPSessionEventLocked sends an ordered session lifecycle event while its caller owns lifecycleMu.
func publishMCPSessionEventLocked(sess *mcpSession, eventType, endReason string) {
	// Session metadata uses only the existing durable lifecycle subject, never OTEL attributes.
	if globalNATSClient != nil && globalNATSClient.JS != nil {
		metadata := mcpSessionMetadata(sess)
		lastActivityAt := mcpSessionLastActivity(sess)
		// Snapshot activity before event time so concurrent traffic cannot produce impossible worker chronology.
		occurredAt := time.Now().UTC()
		eventData, _ := json.Marshal(map[string]any{
			"app_id": sess.appID, "app_token_id": sess.tokenID,
			"session_id": sess.sessionID, "protocol_version": mcpSessionProtocolVersion(sess),
			"type": eventType, "end_reason": endReason, "timestamp": occurredAt,
			"last_activity_at": lastActivityAt,
			"client_name":      metadata.ClientName, "client_version": metadata.ClientVersion,
			"initial_client_ip": metadata.InitialClientIP,
		})
		globalNATSClient.PublishJS(messaging.FusedEngineSessionSubject(sess.appID), eventData)
	}
}

// mcpSessionRegisteredAndActiveLocked checks exact registry ownership while lifecycleMu prevents a concurrent transition.
func mcpSessionRegisteredAndActiveLocked(sess *mcpSession) bool {
	mcpSessions.RLock()
	registered := mcpSessions.m[sess.sessionID] == sess
	mcpSessions.RUnlock()
	// Removed sessions cannot become active again even if their local ended flag has not yet been observed.
	if !registered {
		return false
	}
	sess.activityMu.Lock()
	active := !sess.ended
	sess.activityMu.Unlock()
	return active
}

// mcpSessionProtocolVersion snapshots negotiated state without racing initialization or lifecycle publication.
func mcpSessionProtocolVersion(sess *mcpSession) string {
	sess.activityMu.Lock()
	defer sess.activityMu.Unlock()
	return sess.protocolVersion
}

// touchMCPSession refreshes the idle window and audit timestamp together so cleanup cannot observe half an update.
func touchMCPSession(sess *mcpSession) {
	sess.activityMu.Lock()
	defer sess.activityMu.Unlock()
	// Late response completion must not recreate timer ownership after termination.
	if sess.ended {
		return
	}
	// Retain the monotonic clock internally so wall-clock corrections cannot shorten the idle window.
	sess.lastActivityAt = time.Now()
	// Registration records initial activity before installing its timer.
	if sess.idleTimer != nil {
		sess.idleTimer.Reset(mcpSessionIdleTimeout())
	}
}

// mcpSessionLastActivity projects UTC audit time without discarding the internal monotonic idle clock.
func mcpSessionLastActivity(sess *mcpSession) time.Time {
	sess.activityMu.Lock()
	defer sess.activityMu.Unlock()
	return sess.lastActivityAt.UTC()
}

// setupSSEResponse configures the HTTP response for SSE streaming.
func setupSSEResponse(w http.ResponseWriter, sessionID string) (http.Flusher, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	fmt.Fprintf(w, "event: endpoint\ndata: /mcp/message?sessionId=%s\n\n", sessionID)
	flusher.Flush()
	return flusher, true
}

// processMCPStream forwards one session's child-process responses and keeps
// the SSE connection alive without sharing process state between sessions.
func processMCPStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, stdout io.ReadCloser, sessionID string) {
	stdoutLines := make(chan string)
	var serverFailures <-chan mcpSSEFailure
	// A missing session disables only server-owned events while normal child shutdown still completes safely.
	if sess, ok := lookupMCPSession(sessionID); ok {
		serverFailures = sess.sseFailures
	}
	// The shared pump stops blocked reads and sends when this transport expires.
	go func() {
		defer close(stdoutLines)
		_ = scanMCPResponseLines(ctx, stdout, stdoutLines)
	}()

	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case line, ok := <-stdoutLines:
			// Closed child output ends the transport rather than emitting an empty event.
			if !ok {
				return
			}
			writeMCPSSEMessage(w, flusher, line)
			handleMCPResponse(line, sessionID)
		case failure := <-serverFailures:
			writeMCPSSEMessage(w, flusher, failure.payload)
			terminateMCPSession(sessionID, failure.endReason)
			return
		}
	}
}

// writeMCPSSEMessage keeps child and server-owned JSON-RPC responses on one event framing path.
func writeMCPSSEMessage(w io.Writer, flusher http.Flusher, payload string) {
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	flusher.Flush()
}

// scanMCPResponseLines applies one wire budget to both transports and never
// retains a child line after its session has been canceled.
func scanMCPResponseLines(ctx context.Context, stdout io.ReadCloser, lines chan<- string) error {
	defer stdout.Close()
	// Closing the owned pipe also interrupts Scan when the child has emitted no bytes.
	stopClosing := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stopClosing()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxMCPResponseMessageBytes)
	// A send must remain cancelable when the HTTP consumer has already returned.
	for scanner.Scan() {
		select {
		case lines <- scanner.Text():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Cancellation owns pipe-close errors and must not be reported as a child failure.
	if err := ctx.Err(); err != nil {
		return err
	}
	return scanner.Err()
}

// handleMCPResponse correlates a child JSON-RPC envelope without retaining its content.
func handleMCPResponse(line, sessionID string) {
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	// Malformed child output cannot safely identify a pending request.
	if json.Unmarshal([]byte(line), &msg) != nil {
		return
	}
	// Notifications carry no request ID and therefore complete no tool call.
	if len(msg.ID) == 0 {
		return
	}
	sess, ok := lookupMCPSession(sessionID)
	// A response after session teardown has already had its search span drained.
	if !ok {
		return
	}
	commitMCPSSEInitializeResponse(sess, line, msg.ID)
	completeMCPToolCall(sess, string(compactJSON(msg.ID)), line, "")
}

// commitMCPSSEInitializeResponse turns only the pending successful child handshake into initialized state.
func commitMCPSSEInitializeResponse(sess *mcpSession, response string, responseID json.RawMessage) {
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	// Terminated or replaced sessions cannot publish a late initialization transition.
	if !mcpSessionRegisteredAndActiveLocked(sess) {
		return
	}
	requestID := string(compactJSON(responseID))
	// Only the exact request retained at dispatch may complete SSE negotiation.
	if sess.initializeRequestID == "" || sess.initializeRequestID != requestID {
		return
	}
	// A matching child response consumes this attempt even when its result is invalid.
	sess.initializeRequestID = ""
	protocolVersion, valid := mcpInitializeResultProtocolVersion(response)
	// Error or malformed initialize results leave the session uninitialized and publish no success event.
	if !valid {
		return
	}
	commitMCPInitializationLocked(sess, protocolVersion)
}

// mcpMessageHandler accepts JSON-RPC messages for an authenticated SSE session.
func mcpMessageHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sandbox.mcp.sse_message")
	defer span.End()
	span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("mcp.transport", "sse"))
	// Legacy messages share the same browser-origin boundary as session establishment.
	if !admitMCPRequestOrigin(w, r) {
		recordMCPTransportOutcome(span, "origin_denied", true)
		return
	}
	sessionID := r.URL.Query().Get("sessionId")
	// Legacy SSE messages require the opaque route correlation key.
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId query param required")
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	sess, ok := lookupMCPSession(sessionID)
	// Cross-transport session reuse fails before reading caller content.
	if !ok || sess.transport != "sse" {
		failure := mcpUnavailableSessionFailure()
		writeMCPSessionHTTPError(w, http.StatusNotFound, "mcp session not found or expired", failure)
		recordMCPTransportFailure(span, "denied", failure)
		return
	}
	span.SetAttributes(attribute.String("app.id", sess.appID))
	// The bearer proves ownership because a session ID may appear in access logs.
	if !messageUsesSessionToken(r, sess) {
		writeError(w, http.StatusUnauthorized, "invalid Authorization header")
		recordMCPTransportOutcome(span, "denied", true)
		return
	}
	// Existing per-app message limits remain authoritative for search and execute.
	if !allowMessage(w, sess.appID) {
		recordMCPTransportOutcome(span, "rate_limited", true)
		return
	}
	body, err := readBoundedMCPMessageBody(w, r)
	// The bounded reader owns its safe HTTP failure response.
	if err != nil {
		recordMCPMessageLimit(ctx, sess.appID, "sse", err)
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	dispatchMCPSSEMessage(ctx, span, w, sess, body)
}

// dispatchMCPSSEMessage owns child delivery, timeout tracking, and uncertain-outcome recovery for one admitted message.
func dispatchMCPSSEMessage(ctx context.Context, span trace.Span, w http.ResponseWriter, sess *mcpSession, body []byte) {
	var request mcpJSONRPCRequest
	parsed := json.Unmarshal(body, &request) == nil
	// Legacy POSTs share one child stdin, so only the bounded write is serialized while responses remain asynchronous.
	sess.requestMu.Lock()
	callID, written, writeErr, active := writeMCPChildRequestLocked(ctx, sess, body, request)
	sess.requestMu.Unlock()
	// A queued legacy message must not dispatch after DELETE, revocation, or deactivation wins lifecycle ownership.
	if !active {
		failure := mcpUnavailableSessionFailure()
		if parsed {
			writeMCPJSONRPCSessionError(w, request.ID, errMCPSessionUnavailable.Error(), http.StatusNotFound, failure)
		} else {
			writeMCPSessionHTTPError(w, http.StatusNotFound, errMCPSessionUnavailable.Error(), failure)
		}
		recordMCPTransportFailure(span, "denied", failure)
		return
	}
	if writeErr != nil {
		// A failed child write completes the observed search at this boundary.
		failure := mcpDispatchFailure(request, written)
		completeMCPToolCall(sess, callID, "", "dispatch_failed")
		queued := false
		// Only a parsed, correlatable request may receive a JSON-RPC failure on the event stream.
		if parsed {
			queued = queueMCPSSEFailure(sess, request.ID, "MCP runtime request dispatch failed", failure, "runtime_failed")
		}
		// A parsed request preserves correlation in the POST failure body for clients that surface transport details.
		if parsed {
			writeMCPJSONRPCSessionError(w, request.ID, "MCP runtime request dispatch failed", http.StatusBadGateway, failure)
		} else {
			writeMCPSessionHTTPError(w, http.StatusBadGateway, "MCP runtime request dispatch failed", failure)
		}
		recordMCPTransportFailure(span, "dispatch_failed", failure)
		// Queue saturation or an absent stream cannot leave a failed runtime active.
		if !queued {
			terminateMCPSession(sess.sessionID, "runtime_failed")
		}
		return
	}
	// Only successfully dispatched correlatable requests own the child-response deadline.
	if callID != "" {
		go enforceToolCallTimeout(sess, callID)
	}
	w.WriteHeader(http.StatusAccepted)
	recordMCPTransportOutcome(span, "accepted", false)
}

// queueMCPSSEFailure delivers one compact correlated failure without waiting for an event-stream consumer.
func queueMCPSSEFailure(sess *mcpSession, id json.RawMessage, message string, failure mcpSessionFailureData, endReason string) bool {
	requestID := compactJSON(id)
	// Notifications and null identifiers cannot receive a correlated JSON-RPC response.
	if len(requestID) == 0 || string(requestID) == "null" {
		return false
	}
	payload, err := json.Marshal(mcpJSONRPCErrorEnvelope(requestID, mcpJSONRPCTransportErrorCode, message, failure))
	// Invalid caller correlation must fail closed without exposing an uncorrelated event.
	if err != nil {
		return false
	}
	// Non-blocking queue admission keeps the POST response useful when the event-stream consumer is absent or stalled.
	select {
	case sess.sseFailures <- mcpSSEFailure{payload: string(payload), endReason: endReason}:
		// A short fallback retires synthetic or disconnected sessions whose queue has no active consumer.
		time.AfterFunc(mcpSSEFailureDrainGrace, func() { terminateMCPSession(sess.sessionID, endReason) })
		return true
	default:
		// The caller terminates immediately when bounded delivery cannot be admitted.
		return false
	}
}

// writeMCPChildRequestLocked admits and writes one request while its caller owns requestMu.
func writeMCPChildRequestLocked(ctx context.Context, sess *mcpSession, body []byte, request mcpJSONRPCRequest) (string, int, error, bool) {
	// Lifecycle ownership makes the active-state decision and child write indivisible from termination.
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	// Removed state must fail before telemetry tracking or any child bytes can be written.
	if !mcpSessionRegisteredAndActiveLocked(sess) {
		return "", 0, nil, false
	}
	touchMCPSession(sess)
	// Client attribution is admitted at request receipt, while lifecycle initialization waits for the child result.
	captureMCPClientInfoLocked(ctx, request, sess)
	rememberMCPSSEInitializeRequestLocked(sess, request)
	callID := trackMCPToolCall(ctx, request, sess)
	payload := append(body, '\n')
	written, writeErr := sess.stdin.Write(payload)
	// A partial write without an explicit error can still expose an incomplete envelope to the runtime.
	if writeErr == nil && written != len(payload) {
		writeErr = io.ErrShortWrite
	}
	return callID, written, writeErr, true
}

// rememberMCPSSEInitializeRequestLocked retains one opaque correlation key until the child answers.
func rememberMCPSSEInitializeRequestLocked(sess *mcpSession, request mcpJSONRPCRequest) {
	// Streamable HTTP waits synchronously and therefore needs no retained initialize key.
	if sess.transport != "sse" || request.Method != "initialize" || sess.initialized || sess.initializeRequestID != "" {
		return
	}
	requestID := string(compactJSON(request.ID))
	// Notifications and null identifiers cannot establish an MCP session.
	if requestID == "" || requestID == "null" {
		return
	}
	sess.initializeRequestID = requestID
}

func messageUsesSessionToken(r *http.Request, sess *mcpSession) bool {
	token, ok := extractBearerAuthToken(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	// A session ID can appear in a URL or log, so the bearer still has to prove
	// ownership of the authenticated SSE session before dispatch is allowed.
	return subtle.ConstantTimeCompare([]byte(token), []byte(sess.token)) == 1
}

// readBoundedMCPMessageBody admits one complete envelope with a stable size failure.
func readBoundedMCPMessageBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPMessageBodyBytes))
	// The complete body must fit before any protocol or operation handling begins.
	if err == nil {
		return body, nil
	}
	var maxErr *http.MaxBytesError
	// Only the typed size failure receives the stable payload-limit code.
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "mcp_message_payload_too_large")
		return nil, err
	}
	writeError(w, http.StatusBadRequest, "mcp_message_body_read_failed")
	return nil, err
}

// contextWithMCPIdempotencyIdentity generates a fresh idempotency key and
// request body hash for one MCP tool call, mirroring what the generated SDK
// client does automatically for every gRPC Execute call (ensureIdempotencyKey()
// + requestBodyHash() in base_client.ts). This hosted MCP proxy dispatches
// in-process instead of through that generated client, so without this, every
// MCP-originated call would carry no idempotency identity at all --
// disabling POST/PATCH retry safety (methodRequiresIdempotencyKeyForRetry in
// the engine package) and the idempotency cache (idempotency_cache.go) for
// the entire MCP surface.
//
// The key is fresh per incoming tools/call HTTP request, not per logical
// intent -- exactly like the generated SDK client's behavior: it's stable
// for Engine's own bounded retry loop within this one call, but a client
// resending the same tool call as a new HTTP request gets a new key, same as
// a caller invoking the generated SDK function a second time would.
func contextWithMCPIdempotencyIdentity(ctx context.Context, params map[string]any) context.Context {
	key := uuid.New().String()
	ctx = contextWithExecutionIdentity(ctx, key, hashRequestBody(params))
	return engine.ContextWithIdempotencyKeyPresent(ctx, true)
}

// hashRequestBody returns the SHA-256 hex digest of params' JSON encoding.
// encoding/json sorts map keys before marshaling, so this is deterministic
// for a given params map regardless of Go's map iteration order.
func hashRequestBody(params map[string]any) string {
	encoded, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// enforceToolCallTimeout terminates an SSE session whose child runtime never
// completes a tracked tool call, preventing pending work from defeating idle cleanup.
func enforceToolCallTimeout(sess *mcpSession, callID string) {
	timer := time.NewTimer(time.Duration(cfg.Sandbox.ToolCallTimeoutSeconds) * time.Second)
	defer timer.Stop()
	var sessionDone <-chan struct{}
	// Production timers must not retain a session after authentication or explicit cleanup ends it.
	if sess.lifecycleCtx != nil {
		sessionDone = sess.lifecycleCtx.Done()
	}
	select {
	case <-timer.C:
	case <-sessionDone:
		return
	}
	sess.pendingMu.Lock()
	_, stillPending := sess.pendingRequests[callID]
	mayExecuteProvider := sess.pendingMayExecute[callID]
	sess.pendingMu.Unlock()
	// Only the timeout winner closes telemetry and terminates the stuck session.
	if stillPending {
		failure := mcpRuntimeFailureForExecutionCapability(mayExecuteProvider, mcpRuntimeResponseFailedCode, "runtime_response", "dispatched")
		// Completion ownership prevents a late timeout from racing a child response onto the stream.
		if !completeMCPToolCall(sess, callID, "", "tool_call_timeout") {
			return
		}
		// Failure delivery is terminal because the child outcome cannot be recovered on this session.
		if !queueMCPSSEFailure(sess, json.RawMessage(callID), "MCP runtime response unavailable", failure, "tool_call_timeout") {
			terminateMCPSession(sess.sessionID, "tool_call_timeout")
		}
	}
}

// trackPendingRequest decodes only the correlation envelope for timeout and idle-activity tests.
func trackPendingRequest(ctx context.Context, body []byte, sess *mcpSession) string {
	var request mcpJSONRPCRequest
	// Malformed envelopes remain the child protocol's responsibility and create no tracked call.
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return trackMCPToolCall(ctx, request, sess)
}

// trackMCPToolCall retains one opaque request key and starts search telemetry only for search_docs.
func trackMCPToolCall(ctx context.Context, request mcpJSONRPCRequest, sess *mcpSession) string {
	// Notifications and non-tool methods do not participate in tool timeout or search telemetry.
	if request.Method != "tools/call" || len(request.ID) == 0 {
		return ""
	}
	callID := string(compactJSON(request.ID))
	// JSON null is not a correlatable request identity.
	if callID == "null" {
		return ""
	}
	sess.pendingMu.Lock()
	sess.pendingRequests[callID] = struct{}{}
	// Timeout handling retains only provider capability, never request parameters or content.
	if sess.pendingMayExecute == nil {
		sess.pendingMayExecute = make(map[string]bool)
	}
	sess.pendingMayExecute[callID] = mcpRequestMayExecuteProvider(request)
	// The observer is nil for execute and all future tools, avoiding unrelated spans.
	if observation := startMCPSearchObservation(ctx, request, sess); observation != nil {
		// Directly constructed test or legacy sessions lazily gain the same bounded state.
		if sess.searchTelemetry == nil {
			sess.searchTelemetry = make(map[string]*mcpSearchObservation)
		}
		sess.searchTelemetry[callID] = observation
	}
	sess.pendingMu.Unlock()
	return callID
}

// completeMCPToolCall closes timeout ownership and audits trusted runtime outcomes exactly once.
func completeMCPToolCall(sess *mcpSession, callID, response, errorCode string) bool {
	// Notifications and malformed calls have no state to complete.
	if callID == "" {
		return false
	}
	sess.pendingMu.Lock()
	_, found := sess.pendingRequests[callID]
	// Only the first completion removes timeout ownership.
	if found {
		delete(sess.pendingRequests, callID)
		delete(sess.pendingMayExecute, callID)
		// Refresh before releasing pending ownership so idle cleanup cannot race the completion.
		touchMCPSession(sess)
	}
	observation := sess.searchTelemetry[callID]
	delete(sess.searchTelemetry, callID)
	sess.pendingMu.Unlock()
	// Search telemetry ends once even if timeout and response race each other.
	if observation != nil {
		finishMCPSearchObservation(observation, response, errorCode)
	}
	// Both transports converge here so timeout/response races cannot duplicate delivery audits.
	if found {
		recordMCPRuntimeOutputLimit(context.Background(), sess, response)
		recordMCPRuntimeResultDelivery(context.Background(), sess, response)
	}
	return found
}
