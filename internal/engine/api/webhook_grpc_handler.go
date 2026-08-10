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
// from the stream's Recv loop goroutine (processGRPCWebhookEventLoop).
// Keeping synchronization here prevents concurrent map access under load.
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

// SubscribeWebhooks binds an authenticated app to a receiver-scoped durable
// JetStream consumer and carries delivery acknowledgements over its existing
// bidirectional gRPC channel.
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

	// Confirm activation on the stream so the caller need not infer readiness
	// from server-side logs or the arrival of its first event.
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

// authenticateWebhookSubscribe validates the app ID and family token from
// gRPC metadata. Keeping identity outside the first payload frame applies the
// same authentication contract as every other EngineGRPCServer method.
func (s *EngineGRPCServer) authenticateWebhookSubscribe(ctx context.Context) (uuid.UUID, uuid.UUID, error) {
	appIDString := strings.TrimSpace(grpcAppID(ctx))
	if appIDString == "" {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "x-app-id metadata is required")
	}
	appID, err := uuid.Parse(appIDString)
	if err != nil {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "invalid x-app-id format")
	}
	identity, err := s.tokenValidator.Validate(ctx, appID, grpcAPIKey(ctx))
	if err != nil || identity.AccountID == uuid.Nil {
		return uuid.Nil, uuid.Nil, status.Error(codes.Unauthenticated, "unauthorized")
	}
	return appID, identity.AccountID, nil
}

// setupGRPCWebhookConsumer gives each receiver a stable durable name and queue
// group so reconnects preserve explicit acknowledgement and redelivery state.
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

// handleGRPCNatsMessage maps one durable delivery to the public gRPC envelope.
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

// processGRPCWebhookEventLoop applies client acknowledgements to their pending
// JetStream messages so delivery state advances only after client processing.
func processGRPCWebhookEventLoop(stream enginev1.EngineService_SubscribeWebhooksServer, msgMap *pendingWebhookMsgs, accountID uuid.UUID) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil // Client closure ends this receiver session cleanly.
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
