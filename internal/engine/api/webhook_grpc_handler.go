package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pendingWebhookMsgs guards the ack/nack lookup map that's written from the
// NATS QueueSubscribe callback goroutine (handleGRPCNatsMessage) and read
// from the stream's Recv loop goroutine (processGRPCWebhookEventLoop) --
// two different goroutines touching a plain map without a mutex is a data
// race (the WS path has the same shape via websocket_handler.go's msgMap,
// but that path is being retired, not extended, so it isn't worth changing
// there too).
type pendingWebhookMsgs struct {
	mu   sync.Mutex
	msgs map[string]*nats.Msg
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

// SubscribeWebhooks is the gRPC counterpart to SDKWebSocketHandler
// (websocket_handler.go). It reuses the exact same NATS JetStream durable
// consumer / queue-group setup (setupWebhookConsumer's logic, ported below
// as setupGRPCWebhookConsumer) and the exact same auth/label/filter helpers
// (validateRequestedEvents, resolveWebhookAttachmentLabel, buildFilterSubjects
// -- all transport-agnostic already) -- only the wire framing changes, from
// conn.WriteJSON/conn.ReadJSON to stream.Send/stream.Recv, because this
// stream now rides the same gRPC channel every generated SDK already opens
// for Execute, instead of a second wss://.../sdks/ws WebSocket connection.
func (s *EngineGRPCServer) SubscribeWebhooks(stream enginev1.EngineService_SubscribeWebhooksServer) error {
	ctx := stream.Context()
	thread := observability.ThreadFromContext(ctx)
	subscribeMsg, err := receiveWebhookSubscribe(stream)
	if err != nil {
		return err
	}

	sdkUUID, accountID, err := s.authenticateWebhookSubscribe(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}

	if err := s.store.VerifyWorkspaceOwner(ctx, accountID); err != nil {
		return status.Error(codes.Internal, "failed to resolve workspace")
	}

	validEvents := validateRequestedEvents(ctx, s.store, subscribeMsg.GetEvents())
	webhookLabel, err := resolveWebhookAttachmentLabel(ctx, s.configStore, s.store, sdkUUID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to resolve webhook attachment label", slog.Any("error", err))
	}

	slog.InfoContext(ctx, "SDK connected via gRPC for webhooks",
		slog.String("receiverName", subscribeMsg.GetReceiverName()),
		slog.Any("events", validEvents),
		slog.String("webhookLabel", webhookLabel))
	thread.Step("SDK connected via gRPC for webhooks").AddContext(map[string]any{
		"receiverName": subscribeMsg.GetReceiverName(),
		"events":       validEvents,
		"webhookLabel": webhookLabel,
	}).Success(ctx)

	if err := s.natsClient.InitStream("WEBHOOKS", []string{"webhooks.>"}); err != nil {
		slog.ErrorContext(ctx, "Failed to init WEBHOOKS stream", slog.Any("error", err))
		thread.Step("Failed to init WEBHOOKS stream").Error(ctx, err)
		return status.Error(codes.Internal, "internal error")
	}

	msgMap := newPendingWebhookMsgs()
	filterSubjects := buildFilterSubjects(accountID, webhookLabel, validEvents)

	sub, err := setupGRPCWebhookConsumer(ctx, thread, s.natsClient.JS, accountID, subscribeMsg.GetReceiverName(), filterSubjects, s.natsClient, stream, msgMap)
	if err != nil {
		return status.Error(codes.Internal, "internal error")
	}
	if sub != nil {
		defer sub.Unsubscribe()
	}

	// Confirms the subscription took effect -- the client's SubscribeWebhooks
	// equivalent of the WS path's "SDK connected" log, but observable by the
	// caller itself instead of only server-side logs.
	if err := stream.Send(&enginev1.WebhookServerMessage{
		Payload: &enginev1.WebhookServerMessage_Subscribed{
			Subscribed: &enginev1.WebhookSubscribeAck{ReceiverName: subscribeMsg.GetReceiverName()},
		},
	}); err != nil {
		return err
	}

	return processGRPCWebhookEventLoop(stream, msgMap, accountID)
}

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

// authenticateWebhookSubscribe validates the (artifact_id, token) pair from
// call metadata -- x-api-key/x-artifact-id, the same two values
// authenticateWebSocket reads from HTTP headers for the WS path, just
// carried as gRPC metadata instead so this RPC authenticates exactly like
// every other one on EngineGRPCServer (see grpcAPIKey in engine_grpc.go)
// rather than via fields on the first stream message. auth.TokenValidator's
// Validate is the right tool here per auth.go's own doc comment: it is scoped
// to exactly this artifactID+token shape, unlike the account-level local
// access middleware used by the REST/GraphQL proxy handlers.
func (s *EngineGRPCServer) authenticateWebhookSubscribe(ctx context.Context) (uuid.UUID, uuid.UUID, error) {
	artifactIDStr := strings.TrimSpace(grpcArtifactID(ctx))
	if artifactIDStr == "" {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "x-artifact-id metadata is required")
	}
	sdkUUID, err := uuid.Parse(artifactIDStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "invalid x-artifact-id format")
	}
	accountID, err := s.tokenValidator.Validate(ctx, sdkUUID, grpcAPIKey(ctx))
	if err != nil || accountID == uuid.Nil {
		return uuid.Nil, uuid.Nil, status.Error(codes.Unauthenticated, "unauthorized")
	}
	return sdkUUID, accountID, nil
}

// setupGRPCWebhookConsumer mirrors setupWebhookConsumer (websocket_handler.go)
// exactly -- same durable name, same queue group, same ack policy/max-deliver
// -- only the per-message callback bridges to a gRPC stream instead of a
// websocket.Conn. Keeping this as its own function (rather than
// parameterizing setupWebhookConsumer over an interface) avoids touching the
// already-working, already-tested WS path while both transports coexist
// during the migration window.
func setupGRPCWebhookConsumer(ctx context.Context, thread observability.Thread, js nats.JetStreamContext, accountID uuid.UUID, receiverName string, filterSubjects []string, natsClient *messaging.NATSClient, stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs) (*nats.Subscription, error) {
	if len(filterSubjects) == 0 {
		return nil, nil // No subscription needed
	}

	durableName := accountID.String() + "-" + receiverName
	deliverSubject := "deliver." + durableName
	cfg := &nats.ConsumerConfig{
		Durable:        durableName,
		DeliverGroup:   durableName,
		DeliverSubject: deliverSubject,
		FilterSubjects: filterSubjects,
		AckPolicy:      nats.AckExplicitPolicy,
		MaxDeliver:     3,
	}

	_, err := js.AddConsumer("WEBHOOKS", cfg)
	if err != nil {
		_, err = js.UpdateConsumer("WEBHOOKS", cfg)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to create or update multi-subject consumer", slog.Any("error", err))
			thread.Step("Failed to create or update multi-subject consumer").Error(ctx, err)
		}
	}

	sub, err := js.QueueSubscribe(deliverSubject, durableName, func(m *nats.Msg) {
		handleGRPCNatsMessage(ctx, thread, m, accountID, natsClient, stream, msgMap)
	}, nats.Bind("WEBHOOKS", durableName))

	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to webhooks", slog.Any("error", err))
		thread.Step("Failed to subscribe to webhooks").Error(ctx, err)
		return nil, err
	}
	return sub, nil
}

// handleGRPCNatsMessage mirrors handleNatsMessage (websocket_handler.go)
// exactly, swapping conn.WriteJSON for stream.Send.
func handleGRPCNatsMessage(ctx context.Context, thread observability.Thread, m *nats.Msg, accountID uuid.UUID, natsClient *messaging.NATSClient, stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs) {
	// Subject layout: webhooks.<account>.<service>.<label>.<event>
	// (sandbox/webhook.go's publishWebhookEvent) -- parts[3] is the
	// registration label, so the event itself now starts at index 4.
	parts := strings.Split(m.Subject, ".")
	eventName := "ALL"
	if len(parts) > 4 {
		eventName = strings.Join(parts[4:], ".")
	}

	msgID := m.Header.Get("X-Webhook-Msg-ID")
	if msgID == "" {
		msgID = uuid.New().String()
	}

	if meta, err := m.Metadata(); err == nil && meta.NumDelivered >= 3 {
		m.Term()
		publishFailedAnalytics(ctx, accountID, parts, eventName, msgID, m)
		return
	}

	msgMap.Store(msgID, m)

	// Use original enum value as event so SDK maps it back
	// Enum value is "serviceID.eventName" -- parts[2] is still the
	// service segment (unaffected by the label insertion at [3]).
	var enumEvent string
	if len(parts) > 4 {
		enumEvent = parts[2] + "." + eventName
	} else {
		enumEvent = eventName
	}

	err := stream.Send(&enginev1.WebhookServerMessage{
		Payload: &enginev1.WebhookServerMessage_Event{
			Event: &enginev1.WebhookEvent{
				Id:      msgID,
				Event:   enumEvent,
				Payload: m.Data, // already JSON-encoded by publishWebhookEvent
			},
		},
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send webhook over gRPC stream", slog.Any("error", err))
		thread.Step("Failed to send webhook over gRPC stream").Error(ctx, err)
	} else {
		thread.Step("Dispatched webhook to connected SDK").AddContext(map[string]any{"msg_id": msgID, "event": enumEvent}).Success(ctx)
	}
}

// processGRPCWebhookEventLoop mirrors processWebSocketEventLoop
// (websocket_handler.go), reading WebhookAck/WebhookNack messages back from
// the stream instead of {"type": "ack"|"nack", "id": ...} JSON frames.
func processGRPCWebhookEventLoop(stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs, accountID uuid.UUID) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil // client closed the stream -- same as a WS read error breaking the loop
		}

		if ack := msg.GetAck(); ack != nil {
			if m, ok := msgMap.Take(ack.GetEventId()); ok {
				m.Ack()
				publishSuccessAnalytics(stream.Context(), accountID, ack.GetEventId(), m)
			}
		} else if nack := msg.GetNack(); nack != nil {
			if m, ok := msgMap.Take(nack.GetEventId()); ok {
				m.NakWithDelay(time.Second * 5)
			}
		}
	}
}
