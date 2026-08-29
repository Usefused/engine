package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookstream"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	webhookMaxHandlerAttempts           = 3
	webhookBrokerMaxDeliver             = webhookMaxHandlerAttempts + 1
	webhookAuthorizationRecheckInterval = 15 * time.Second
)

// pendingWebhookMsgs guards the ack/nack lookup map that's written from the
// NATS QueueSubscribe callback goroutine (handleGRPCNatsMessage) and read
// from the stream's Recv loop goroutine (processGRPCWebhookEventLoop).
// Keeping synchronization here prevents concurrent map access under load.
type pendingWebhookMsgs struct {
	mu   sync.Mutex
	msgs map[string]*nats.Msg
}

// webhookSubscriptionScope contains the already-authorized subject union and stable durable identity.
type webhookSubscriptionScope struct {
	appID          uuid.UUID
	tokenID        uuid.UUID
	accountID      uuid.UUID
	appFamilyID    uuid.UUID
	webhookLabel   string
	providerEvents []string
	filterSubjects []string
}

func newPendingWebhookMsgs() *pendingWebhookMsgs {
	return &pendingWebhookMsgs{msgs: make(map[string]*nats.Msg)}
}

func (p *pendingWebhookMsgs) Store(id string, m *nats.Msg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs[id] = m
}

func (p *pendingWebhookMsgs) Take(id string) (*nats.Msg, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.msgs[id]
	if ok {
		delete(p.msgs, id)
	}
	return m, ok
}

// SubscribeWebhooks binds an authenticated app to a receiver-scoped durable
// JetStream consumer and carries delivery acknowledgements over its existing
// bidirectional gRPC channel.
func (s *EngineGRPCServer) SubscribeWebhooks(stream enginev1.EngineService_SubscribeWebhooksServer) error {
	ctx := stream.Context()
	thread := observability.ThreadFromContext(ctx)
	subscribeMsg, err := receiveWebhookSubscribe(stream)
	// Invalid first-frame subscription identity stops before token or storage access.
	if err != nil {
		return err
	}
	scope, err := s.resolveWebhookSubscriptionScope(ctx, subscribeMsg.GetEvents())
	// Runtime authentication and exact immutable selections must settle before broker state is touched.
	if err != nil {
		return err
	}
	registration, err := s.registerWebhookSubscription(ctx, scope)
	// A token or exact-app invalidation racing initial authorization prevents any broker delivery from starting.
	if err != nil {
		return err
	}
	defer registration.Unregister()

	slog.InfoContext(ctx, "SDK connected via gRPC for webhooks",
		slog.String("receiverName", subscribeMsg.GetReceiverName()),
		slog.Any("events", scope.providerEvents),
		slog.String("webhookLabel", scope.webhookLabel))
	thread.Step("SDK connected via gRPC for webhooks").AddContext(map[string]any{
		"receiverName": subscribeMsg.GetReceiverName(),
		"events":       scope.providerEvents,
		"webhookLabel": scope.webhookLabel,
	}).Success(ctx)

	// Runtime stream reconciliation keeps subscription startup safe after embedded NATS restarts.
	if err := s.natsClient.InitStream("WEBHOOKS", []string{"webhooks.>"}); err != nil {
		slog.ErrorContext(ctx, "Failed to init WEBHOOKS stream", slog.Any("error", err))
		thread.Step("Failed to init WEBHOOKS stream").Error(ctx, err)
		return status.Error(codes.Internal, "internal error")
	}

	msgMap := newPendingWebhookMsgs()
	sub, err := setupGRPCWebhookConsumer(ctx, thread, s.natsClient.JS, scope.accountID, scope.appFamilyID, scope.appID, subscribeMsg.GetReceiverName(), scope.filterSubjects, s.natsClient, stream, msgMap, registration)
	// Consumer setup must complete before readiness is acknowledged to the generated receiver.
	if err != nil {
		return status.Error(codes.Internal, "internal error")
	}
	// A nil subscription is valid when the client registered no authorized subjects.
	if sub != nil {
		defer sub.Unsubscribe()
	}

	// Confirm activation on the stream so the caller need not infer readiness
	// from server-side logs or the arrival of its first event.
	if err := stream.Send(&enginev1.WebhookServerMessage{
		Payload: &enginev1.WebhookServerMessage_Subscribed{
			Subscribed: &enginev1.WebhookSubscribeAck{ReceiverName: subscribeMsg.GetReceiverName()},
		},
	}); err != nil {
		// Failed readiness delivery leaves the durable available for the client's reconnect attempt.
		return err
	}

	return s.processGRPCWebhookEventLoop(stream, msgMap, scope, registration)
}

// resolveWebhookSubscriptionScope authenticates one exact app and constructs its explicit-plus-implicit subject union.
func (s *EngineGRPCServer) resolveWebhookSubscriptionScope(ctx context.Context, requestedEvents []string) (webhookSubscriptionScope, error) {
	identity, err := s.authenticateWebhookIdentity(ctx)
	// Runtime authentication remains the sole source of exact version and stable family identity.
	if err != nil {
		return webhookSubscriptionScope{}, err
	}
	appID, accountID, appFamilyID := identity.AppID, identity.AccountID, identity.AppFamilyID
	// The singleton workspace check prevents a valid token projection from crossing local Engine ownership.
	if err := s.store.VerifyWorkspaceOwner(ctx, accountID); err != nil {
		return webhookSubscriptionScope{}, status.Error(codes.Internal, "failed to resolve workspace")
	}
	runtime, err := s.store.GetAppRuntime(ctx, appID)
	// One exact runtime read supplies provider selection, implicit auth capability, and attachment identity without per-event queries.
	if err != nil || !sameWebhookRuntimeIdentity(runtime, appID, accountID, appFamilyID) {
		return webhookSubscriptionScope{}, status.Error(codes.Internal, "failed to resolve SDK webhook configuration")
	}
	providerRequests, authEventRequests := partitionWebhookEventRequests(requestedEvents)
	providerEvents, err := validateRequestedEvents(runtime, providerRequests)
	// Malformed immutable selections cannot fall back to mutable workspace state for broader provider delivery.
	if err != nil {
		return webhookSubscriptionScope{}, status.Error(codes.Internal, "failed to resolve SDK webhook configuration")
	}
	webhookLabel, labelErr := resolveWebhookAttachmentLabelForRuntime(ctx, s.configStore, runtime)
	// Explicit attachment failure is logged but cannot broaden filters; the empty label yields no provider subjects.
	if labelErr != nil {
		slog.WarnContext(ctx, "Failed to resolve webhook attachment label", slog.Any("error", labelErr))
		webhookLabel = ""
	}
	authSubjects, err := authEventFilterSubjectsForRuntime(runtime, accountID, appFamilyID, authEventRequests)
	// Reserved Fused auth requests fail closed when the exact SDK version did not select that connected-auth service.
	if err != nil {
		return webhookSubscriptionScope{}, status.Error(codes.PermissionDenied, "requested Fused auth event is not available to this SDK version")
	}
	providerSubjects := buildFilterSubjects(accountID, webhookLabel, providerEvents)
	return webhookSubscriptionScope{
		appID: appID, tokenID: identity.TokenID, accountID: accountID, appFamilyID: appFamilyID, webhookLabel: webhookLabel,
		providerEvents: providerEvents, filterSubjects: append(providerSubjects, authSubjects...),
	}, nil
}

// sameWebhookRuntimeIdentity checks the already-authenticated workspace and family against the persisted exact app row.
func sameWebhookRuntimeIdentity(runtime *store.AppRuntime, appID, accountID, appFamilyID uuid.UUID) bool {
	// Nil or divergent runtime state is an internal authorization invariant failure, never a broader fallback.
	if runtime == nil {
		return false
	}
	return runtime.AppID == appID && runtime.AccountID == accountID && runtime.AppFamilyID == appFamilyID && runtime.Kind == store.AppKindSDK
}

// receiveWebhookSubscribe admits the immutable receiver name and requested event set from the first client frame.
func receiveWebhookSubscribe(stream enginev1.EngineService_SubscribeWebhooksServer) (*enginev1.WebhookSubscribe, error) {
	// Subscription identity is accepted only in the first frame so later ACK
	// traffic cannot silently change the durable consumer's scope.
	first, err := stream.Recv()
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "expected initial subscribe message")
	}
	subscribe := first.GetSubscribe()
	if subscribe == nil {
		return nil, status.Error(codes.InvalidArgument, "first message must be a subscribe")
	}
	if strings.TrimSpace(subscribe.GetReceiverName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "receiver_name is required")
	}
	if len(subscribe.GetEvents()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "events are required")
	}
	return subscribe, nil
}

// authenticateWebhookIdentity validates the app ID and family token from
// gRPC metadata. Keeping identity outside the first payload frame applies the
// same authentication contract as every other EngineGRPCServer method.
func (s *EngineGRPCServer) authenticateWebhookIdentity(ctx context.Context) (auth.RuntimeIdentity, error) {
	appIDString := strings.TrimSpace(grpcAppID(ctx))
	// Runtime subscriptions require the same exact immutable app identity as execution calls.
	if appIDString == "" {
		return auth.RuntimeIdentity{}, status.Error(codes.InvalidArgument, "x-app-id metadata is required")
	}
	appID, err := uuid.Parse(appIDString)
	// Malformed metadata cannot be allowed to fall back to a family or workspace-wide receiver.
	if err != nil {
		return auth.RuntimeIdentity{}, status.Error(codes.InvalidArgument, "invalid x-app-id format")
	}
	identity, err := s.tokenValidator.Validate(ctx, appID, grpcAPIKey(ctx))
	// A family token is accepted only when it authorizes this exact active or deprecated app version.
	if err != nil || identity.AppID != appID || identity.AccountID == uuid.Nil || identity.AppFamilyID == uuid.Nil || identity.TokenID == uuid.Nil || identity.Kind != store.AppKindSDK {
		return auth.RuntimeIdentity{}, status.Error(codes.Unauthenticated, "unauthorized")
	}
	return identity, nil
}

// authenticateWebhookSubscribe preserves the existing identity helper contract for callers that do not manage live registrations.
func (s *EngineGRPCServer) authenticateWebhookSubscribe(ctx context.Context) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	identity, err := s.authenticateWebhookIdentity(ctx)
	// Authentication errors remain unchanged while the registration-aware path retains opaque token identity separately.
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	return identity.AppID, identity.AccountID, identity.AppFamilyID, nil
}

// registerWebhookSubscription fences startup with a second source recheck after volatile registration.
func (s *EngineGRPCServer) registerWebhookSubscription(ctx context.Context, scope webhookSubscriptionScope) (*webhookstream.Registration, error) {
	registration, ok := s.webhookStreams.Register(scope.tokenID, scope.appID)
	// Invalid opaque identity cannot acquire a live registration before any JetStream consumer is attached.
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "webhook subscription token is no longer active")
	}
	// Registration precedes revalidation so an invalidation during the source check closes the generation fence.
	if err := s.revalidateWebhookSubscription(ctx, scope); err != nil {
		registration.Unregister()
		return nil, err
	}
	// A generation change after source validation is treated as stale authorization and must reconnect.
	if !registration.Confirm() {
		return nil, webhookStreamCancellationError(registration.Reason())
	}
	return registration, nil
}

// revalidateWebhookSubscription checks both opaque-token identity and the exact persisted app runtime.
func (s *EngineGRPCServer) revalidateWebhookSubscription(ctx context.Context, scope webhookSubscriptionScope) error {
	identity, err := s.tokenValidator.Validate(ctx, scope.appID, grpcAPIKey(ctx))
	// Stable opaque and relational identity prevents a cached or replaced token from inheriting an open stream.
	if err != nil || !sameWebhookTokenIdentity(identity, scope) {
		return status.Error(codes.PermissionDenied, "webhook subscription authorization is no longer active")
	}
	runtime, err := s.store.GetAppRuntime(ctx, scope.appID)
	// Exact runtime disappearance or family drift is a hard authorization boundary for future deliveries.
	if err != nil || !sameWebhookRuntimeIdentity(runtime, scope.appID, scope.accountID, scope.appFamilyID) {
		return status.Error(codes.PermissionDenied, "webhook subscription app version is no longer active")
	}
	return nil
}

// sameWebhookTokenIdentity compares only stable authorization identity retained by the live stream.
func sameWebhookTokenIdentity(identity auth.RuntimeIdentity, scope webhookSubscriptionScope) bool {
	return identity.AppID == scope.appID && identity.TokenID == scope.tokenID &&
		identity.AccountID == scope.accountID && identity.AppFamilyID == scope.appFamilyID
}

// setupGRPCWebhookConsumer gives each receiver a stable durable name and queue
// group so reconnects preserve explicit acknowledgement and redelivery state.
func setupGRPCWebhookConsumer(ctx context.Context, thread observability.Thread, js nats.JetStreamContext, accountID, appFamilyID, appID uuid.UUID, receiverName string, filterSubjects []string, natsClient *messaging.NATSClient, stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs, registration *webhookstream.Registration) (*nats.Subscription, error) {
	// A receiver with no authorized explicit or implicit event subjects still receives the normal subscription acknowledgement.
	if len(filterSubjects) == 0 {
		return nil, nil // No subscription needed
	}

	// Exact app identity prevents concurrently deployed sibling versions from overwriting incompatible immutable filter sets.
	durableName := webhookDurableName(accountID, appFamilyID, appID, receiverName)
	deliverSubject := "deliver." + durableName
	cfg := &nats.ConsumerConfig{
		Durable:        durableName,
		DeliverGroup:   durableName,
		DeliverSubject: deliverSubject,
		FilterSubjects: filterSubjects,
		AckPolicy:      nats.AckExplicitPolicy,
		// One terminal interception after the public attempts records exhaustion without dispatching a fourth handler call.
		MaxDeliver: webhookBrokerMaxDeliver,
	}

	_, err := js.AddConsumer("WEBHOOKS", cfg)
	// Reconnects update the existing exact-app durable so handler additions take effect without losing pending messages.
	if err != nil {
		_, err = js.UpdateConsumer("WEBHOOKS", cfg)
		// A failed update leaves receiver durability uncertain and must stop stream activation.
		if err != nil {
			slog.ErrorContext(ctx, "Failed to create or update multi-subject consumer", slog.Any("error", err))
			thread.Step("Failed to create or update multi-subject consumer").Error(ctx, err)
			return nil, err
		}
	}

	sub, err := js.QueueSubscribe(deliverSubject, durableName, func(m *nats.Msg) {
		// An invalidated registration must stop public delivery even before the event loop returns and unsubscribes.
		if !registration.IsCurrent() {
			return
		}
		handleGRPCNatsMessage(ctx, thread, m, accountID, natsClient, stream, msgMap)
	}, nats.Bind("WEBHOOKS", durableName))

	// Delivery subscription failure cannot be reported as an active receiver.
	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to webhooks", slog.Any("error", err))
		thread.Step("Failed to subscribe to webhooks").Error(ctx, err)
		return nil, err
	}
	return sub, nil
}

// webhookDurableName isolates receiver state by exact immutable version while subjects remain family-scoped for lifecycle continuity.
func webhookDurableName(accountID, appFamilyID, appID uuid.UUID, receiverName string) string {
	return accountID.String() + "-" + appFamilyID.String() + "-" + appID.String() + "-" + receiverName
}

// handleGRPCNatsMessage maps one durable delivery to the public gRPC envelope.
func handleGRPCNatsMessage(ctx context.Context, thread observability.Thread, m *nats.Msg, accountID uuid.UUID, natsClient *messaging.NATSClient, stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs) {
	parsed, validSubject := messaging.ParseWebhookSubject(m.Subject)
	// Unknown or malformed subjects are terminal because no generated handler can safely identify them.
	if !validSubject {
		_ = m.Term()
		return
	}
	eventName := parsed.EventName

	msgID := m.Header.Get("X-Webhook-Msg-ID")
	// Provider integrations predating stable message IDs remain deliverable with a generated receiver-local identity.
	if msgID == "" {
		msgID = uuid.New().String()
	}

	// Exhausted provider delivery emits its existing analytics, while Fused-owned lifecycle events stay off provider accounting.
	if meta, err := m.Metadata(); err == nil && webhookDeliveryExhausted(meta.NumDelivered) {
		_ = m.Term()
		// Exhausted Fused lifecycle deliveries remain outside provider webhook analytics.
		if shouldPublishWebhookAnalytics(m.Subject) {
			publishFailedAnalytics(ctx, accountID, parsed.ServiceID, eventName, msgID, m)
		}
		return
	}

	msgMap.Store(msgID, m)

	// Both provider and Fused-owned events preserve the generated SDK's established serviceID.eventName contract.
	enumEvent := parsed.ServiceID.String() + "." + eventName

	err := stream.Send(&enginev1.WebhookServerMessage{
		Payload: &enginev1.WebhookServerMessage_Event{
			Event: &enginev1.WebhookEvent{
				Id:      msgID,
				Event:   enumEvent,
				Payload: m.Data, // already JSON-encoded by publishWebhookEvent
			},
		},
	})
	// Failed client writes retain the JetStream message for reconnect redelivery.
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send webhook over gRPC stream", slog.Any("error", err))
		thread.Step("Failed to send webhook over gRPC stream").Error(ctx, err)
	} else {
		// Successful writes are safe to expose as receiver-local delivery progress.
		thread.Step("Dispatched webhook to connected SDK").AddContext(map[string]any{"msg_id": msgID, "event": enumEvent}).Success(ctx)
	}
}

// webhookDeliveryExhausted reserves the broker's final delivery for terminal accounting after three public handler attempts.
func webhookDeliveryExhausted(delivered uint64) bool {
	return delivered > webhookMaxHandlerAttempts
}

// webhookClientReceiveResult transports the single allowed Recv goroutine into a cancellation-aware event loop.
type webhookClientReceiveResult struct {
	message *enginev1.WebhookClientMessage
	err     error
}

// processGRPCWebhookEventLoop applies acknowledgements while periodically recovering from missed best-effort invalidations.
func (s *EngineGRPCServer) processGRPCWebhookEventLoop(stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs, scope webhookSubscriptionScope, registration *webhookstream.Registration) error {
	ticker := time.NewTicker(webhookAuthorizationRecheckInterval)
	defer ticker.Stop()
	return s.processGRPCWebhookEvents(stream, msgMap, scope, registration, receiveWebhookClientMessages(stream), ticker.C)
}

// processGRPCWebhookEvents selects authorization cancellation before accepting further client delivery state.
func (s *EngineGRPCServer) processGRPCWebhookEvents(stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs, scope webhookSubscriptionScope, registration *webhookstream.Registration, received <-chan webhookClientReceiveResult, recheck <-chan time.Time) error {
	// One select owns all state transitions so cancellation and client ACKs cannot race through separate loops.
	for {
		select {
		case <-stream.Context().Done():
			// Client cancellation leaves the exact-app durable available for an authorized reconnect.
			return nil
		case <-registration.Done():
			// Revocation and runtime invalidation terminate before another broker callback can dispatch.
			return webhookStreamCancellationError(registration.Reason())
		case <-recheck:
			// The cache TTL plus this interval bounds recovery when best-effort core NATS invalidation is missed.
			if err := s.recheckWebhookSubscription(stream.Context(), scope, registration); err != nil {
				return err
			}
		case result, open := <-received:
			// A closed receive channel means the client ended its half of the bidirectional stream.
			if !open || result.err != nil {
				return nil
			}
			applyWebhookClientMessage(stream.Context(), result.message, msgMap, scope.accountID)
		}
	}
}

// receiveWebhookClientMessages keeps one Recv active while the owner selects local authorization cancellation.
func receiveWebhookClientMessages(stream enginev1.EngineService_SubscribeWebhooksServer) <-chan webhookClientReceiveResult {
	received := make(chan webhookClientReceiveResult, 1)
	// One goroutine is required because gRPC Recv is blocking while local invalidation must terminate promptly.
	go func() {
		defer close(received)
		// Recv remains serialized as required by the bidirectional gRPC stream contract.
		for {
			message, err := stream.Recv()
			select {
			case received <- webhookClientReceiveResult{message: message, err: err}:
				// A receive error is terminal; the owner preserves the durable for reconnect.
				if err != nil {
					return
				}
			case <-stream.Context().Done():
				// Handler return cancels the stream context and releases a blocked sender.
				return
			}
		}
	}()
	return received
}

// recheckWebhookSubscription makes missed invalidation recovery fail closed before the next client message.
func (s *EngineGRPCServer) recheckWebhookSubscription(ctx context.Context, scope webhookSubscriptionScope, registration *webhookstream.Registration) error {
	if err := s.revalidateWebhookSubscription(ctx, scope); err != nil {
		// Detaching first prevents concurrent broker callbacks from sending under stale authorization.
		registration.Unregister()
		return err
	}
	// An invalidation racing the source reads wins over their stale result.
	if !registration.IsCurrent() {
		return webhookStreamCancellationError(registration.Reason())
	}
	return nil
}

// applyWebhookClientMessage advances one known delivery according to the generated receiver outcome.
func applyWebhookClientMessage(ctx context.Context, message *enginev1.WebhookClientMessage, msgMap *pendingWebhookMsgs, accountID uuid.UUID) {
	// ACK advances a known pending delivery and records provider analytics only when applicable.
	if ack := message.GetAck(); ack != nil {
		acknowledgeWebhookClientMessage(ctx, ack, msgMap, accountID)
		return
	}
	// NACK applies a short broker-owned delay so reconnect loops cannot hot-spin one failing handler.
	if nack := message.GetNack(); nack != nil {
		// Unknown or already-settled IDs cannot schedule duplicate broker delivery.
		if brokerMessage, ok := msgMap.Take(nack.GetEventId()); ok {
			_ = brokerMessage.NakWithDelay(time.Second * 5)
		}
	}
}

// acknowledgeWebhookClientMessage records provider analytics without classifying Fused lifecycle delivery as ingress.
func acknowledgeWebhookClientMessage(ctx context.Context, ack *enginev1.WebhookAck, msgMap *pendingWebhookMsgs, accountID uuid.UUID) {
	brokerMessage, ok := msgMap.Take(ack.GetEventId())
	// Unknown or already-settled IDs cannot mutate durable delivery state twice.
	if !ok {
		return
	}
	_ = brokerMessage.Ack()
	// Provider acknowledgements retain canonical analytics; Fused auth events intentionally do not masquerade as ingress.
	if shouldPublishWebhookAnalytics(brokerMessage.Subject) {
		publishSuccessAnalytics(ctx, accountID, ack.GetEventId(), brokerMessage)
	}
}

// webhookStreamCancellationError returns a stable gRPC status without exposing token or app identity.
func webhookStreamCancellationError(reason webhookstream.CancellationReason) error {
	switch reason {
	case webhookstream.CancellationReasonTokenInvalidated, webhookstream.CancellationReasonRejected:
		// Revoked token identity cannot authorize a retry with the same credentials.
		return status.Error(codes.PermissionDenied, "webhook subscription authorization changed; reconnect with active credentials")
	case webhookstream.CancellationReasonAppInvalidated:
		// Immutable runtime invalidation is retriable; fresh validation decides whether this was an update or hard deactivation.
		return status.Error(codes.Unavailable, "webhook app runtime changed; reconnect")
	default:
		// Unexpected local teardown is retriable but must never continue delivery on the old stream.
		return status.Error(codes.Unavailable, "webhook subscription ended; reconnect")
	}
}
