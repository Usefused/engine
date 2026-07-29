package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // In production, restrict this
	},
}

type wsInitMsg struct {
	Type         string   `json:"type"`
	ReceiverName string   `json:"receiverName"`
	Events       []string `json:"events"`
}

func authenticateWebSocket(ctx context.Context, r *http.Request, validator auth.TokenValidator) (uuid.UUID, uuid.UUID, error) {
	apiKey := r.Header.Get("X-API-Key")
	if apiKey == "" {
		apiKey = r.URL.Query().Get("apiKey")
	}

	artifactIDStr := r.Header.Get("X-Artifact-ID")
	if artifactIDStr == "" {
		artifactIDStr = r.URL.Query().Get("artifactId")
	}

	if artifactIDStr == "" {
		return uuid.Nil, uuid.Nil, errors.New("missing sdk id")
	}

	sdkUUID, err := uuid.Parse(artifactIDStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, errors.New("invalid sdk id format")
	}

	if validator == nil {
		return uuid.Nil, uuid.Nil, errors.New("missing validator")
	}

	accountID, err := validator.Validate(ctx, sdkUUID, apiKey)
	if err != nil || accountID == uuid.Nil {
		return uuid.Nil, uuid.Nil, errors.New("unauthorized")
	}

	return sdkUUID, accountID, nil
}

func parseInitMessage(conn *websocket.Conn) (wsInitMsg, error) {
	var initMsg wsInitMsg
	err := conn.ReadJSON(&initMsg)
	if err != nil || initMsg.Type != "init" {
		return initMsg, errors.New("invalid init message")
	}
	if initMsg.ReceiverName == "" {
		return initMsg, errors.New("receiverName is required")
	}
	if len(initMsg.Events) == 0 {
		return initMsg, errors.New("events are required")
	}
	return initMsg, nil
}

func validateRequestedEvents(ctx context.Context, s store.Store, events []string) []string {
	validEvents := make([]string, 0)
	for _, ev := range events {
		if ev == "ALL" {
			validEvents = append(validEvents, ev)
			continue
		}
		parts := strings.Split(ev, ".")
		if len(parts) >= 2 {
			serviceID, err := uuid.Parse(parts[0])
			if err != nil {
				continue
			}
			enabled, err := s.IsWorkspaceServiceEnabled(ctx, serviceID)
			if err == nil && enabled {
				validEvents = append(validEvents, ev)
				continue
			}
			webhooks, err := s.ListWorkspaceWebhooks(ctx, serviceID)
			if err == nil && len(webhooks) > 0 {
				validEvents = append(validEvents, ev)
			} else {
				slog.WarnContext(ctx, "Workspace service not enabled and no webhooks configured", slog.String("serviceID", serviceID.String()))
			}
		}
	}
	return validEvents
}

func buildFilterSubjects(accountID uuid.UUID, webhookLabel string, validEvents []string) []string {
	var filterSubjects []string
	if webhookLabel != "" {
		for _, ev := range validEvents {
			if ev == "ALL" {
				continue
			}
			serviceID, eventName, found := strings.Cut(ev, ".")
			if !found {
				continue
			}
			filterSubjects = append(filterSubjects, "webhooks."+accountID.String()+"."+serviceID+"."+subjectSafeLabel(webhookLabel)+"."+eventName)
		}
	}
	return filterSubjects
}

func publishFailedAnalytics(accountID uuid.UUID, parts []string, eventName string, msgID string, m *nats.Msg, natsClient *messaging.NATSClient) {
	serviceIDStr := ""
	if len(parts) >= 2 {
		serviceIDStr = parts[1]
	}
	latencyMs := computeLatencyMs(m.Header.Get("X-Webhook-Start-Time"))
	analyticsPayload, _ := json.Marshal(map[string]any{
		"msg_id":           msgID,
		"account_id":       accountID.String(),
		"service_id":       serviceIDStr,
		"event_name":       eventName,
		"status":           "failed",
		"payload_size":     len(m.Data),
		"latency_ms":       latencyMs,
		"credits_consumed": 0,
		"timestamp":        time.Now(),
	})
	natsClient.PublishJS("webhook.analytics.failed", analyticsPayload)
}

func publishSuccessAnalytics(accountID uuid.UUID, msgID string, m *nats.Msg, natsClient *messaging.NATSClient) {
	parts := strings.Split(m.Subject, ".")
	serviceIDStr := ""
	if len(parts) >= 2 {
		serviceIDStr = parts[1]
	}
	eventName := "ALL"
	if len(parts) > 4 {
		eventName = strings.Join(parts[4:], ".")
	}
	latencyMs := computeLatencyMs(m.Header.Get("X-Webhook-Start-Time"))
	analyticsPayload, _ := json.Marshal(map[string]any{
		"msg_id":           msgID,
		"account_id":       accountID.String(),
		"service_id":       serviceIDStr,
		"event_name":       eventName,
		"status":           "success",
		"payload_size":     len(m.Data),
		"latency_ms":       latencyMs,
		"credits_consumed": 0,
		"timestamp":        time.Now(),
	})
	natsClient.PublishJS("webhook.analytics.success", analyticsPayload)
}

func handleNatsMessage(ctx context.Context, thread observability.Thread, m *nats.Msg, accountID uuid.UUID, natsClient *messaging.NATSClient, conn *websocket.Conn, msgMap map[string]*nats.Msg) {
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
		publishFailedAnalytics(accountID, parts, eventName, msgID, m, natsClient)
		return
	}

	msgMap[msgID] = m
	var rawPayload any
	_ = json.Unmarshal(m.Data, &rawPayload)

	// Use original enum value as event so SDK maps it back
	// Enum value is "serviceID.eventName" -- parts[2] is still the
	// service segment (unaffected by the label insertion at [3]).
	var enumEvent string
	if len(parts) > 4 {
		enumEvent = parts[2] + "." + eventName
	} else {
		enumEvent = eventName
	}

	outgoing := map[string]any{
		"type":    "webhook",
		"event":   enumEvent,
		"id":      msgID,
		"payload": rawPayload,
	}

	err := conn.WriteJSON(outgoing)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to write webhook to WS", slog.Any("error", err))
		thread.Step("Failed to write webhook to WS").Error(ctx, err)
	} else {
		thread.Step("Dispatched webhook to connected SDK").AddContext(map[string]any{"msg_id": msgID, "event": enumEvent}).Success(ctx)
	}
}

func setupWebhookConsumer(ctx context.Context, thread observability.Thread, js nats.JetStreamContext, accountID uuid.UUID, receiverName string, filterSubjects []string, natsClient *messaging.NATSClient, conn *websocket.Conn, msgMap map[string]*nats.Msg) (*nats.Subscription, error) {
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
		handleNatsMessage(ctx, thread, m, accountID, natsClient, conn, msgMap)
	}, nats.Bind("WEBHOOKS", durableName))

	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to webhooks", slog.Any("error", err))
		thread.Step("Failed to subscribe to webhooks").Error(ctx, err)
		return nil, err
	}
	return sub, nil
}

func processWebSocketEventLoop(conn *websocket.Conn, msgMap map[string]*nats.Msg, accountID uuid.UUID, natsClient *messaging.NATSClient) {
	for {
		var msg struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		err := conn.ReadJSON(&msg)
		if err != nil {
			break
		}

		if msg.Type == "ack" {
			if m, ok := msgMap[msg.ID]; ok {
				m.Ack()
				delete(msgMap, msg.ID)
				publishSuccessAnalytics(accountID, msg.ID, m, natsClient)
			}
		} else if msg.Type == "nack" {
			if m, ok := msgMap[msg.ID]; ok {
				m.NakWithDelay(time.Second * 5)
				delete(msgMap, msg.ID)
			}
		}
	}
}

// SDKWebSocketHandler upgrades the connection and bridges NATS JS to the WebSocket.
func SDKWebSocketHandler(configStore store.ConfigRepository, s store.Store, natsClient *messaging.NATSClient, validator auth.TokenValidator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		thread := observability.ThreadFromContext(ctx)

		sdkUUID, accountID, err := authenticateWebSocket(ctx, r, validator)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to upgrade websocket", slog.Any("error", err))
			thread.Step("Failed to upgrade websocket").Error(ctx, err)
			return
		}
		defer conn.Close()

		initMsg, err := parseInitMessage(conn)
		if err != nil {
			slog.ErrorContext(ctx, "Init message error", slog.Any("error", err))
			thread.Step("Init message error").Error(ctx, err)
			conn.WriteJSON(map[string]string{"error": err.Error()})
			return
		}

		slog.InfoContext(ctx, "Received init msg", slog.Any("rawEvents", initMsg.Events))

		if err := s.VerifyWorkspaceOwner(ctx, accountID); err != nil {
			slog.ErrorContext(ctx, "Failed to get workspace for account", slog.Any("error", err))
			conn.WriteJSON(map[string]string{"error": "internal error"})
			return
		}

		validEvents := validateRequestedEvents(ctx, s, initMsg.Events)
		webhookLabel, err := resolveWebhookAttachmentLabel(ctx, configStore, s, sdkUUID)
		if err != nil {
			slog.WarnContext(ctx, "Failed to resolve webhook attachment label", slog.Any("error", err))
		}

		slog.InfoContext(ctx, "SDK connected via WebSocket for webhooks", slog.String("receiverName", initMsg.ReceiverName), slog.Any("events", validEvents), slog.String("webhookLabel", webhookLabel))
		thread.Step("SDK connected via WebSocket for webhooks").AddContext(map[string]any{"receiverName": initMsg.ReceiverName, "events": validEvents, "webhookLabel": webhookLabel}).Success(ctx)

		if err := natsClient.InitStream("WEBHOOKS", []string{"webhooks.>"}); err != nil {
			slog.ErrorContext(ctx, "Failed to init WEBHOOKS stream", slog.Any("error", err))
			thread.Step("Failed to init WEBHOOKS stream").Error(ctx, err)
			conn.WriteJSON(map[string]string{"error": "internal error"})
			return
		}

		msgMap := make(map[string]*nats.Msg)
		filterSubjects := buildFilterSubjects(accountID, webhookLabel, validEvents)

		sub, err := setupWebhookConsumer(ctx, thread, natsClient.JS, accountID, initMsg.ReceiverName, filterSubjects, natsClient, conn, msgMap)
		if err != nil {
			return
		}
		if sub != nil {
			defer sub.Unsubscribe()
		}

		processWebSocketEventLoop(conn, msgMap, accountID, natsClient)
	}
}

// resolveWebhookAttachmentLabel looks up which kind: webhook artifact (if
// any) artifactID's own config attached, entirely server-side -- the connecting
// client (generated SDK/MCP code) never has to know or report this itself.
// fused_artifact_scopes.config_key (set at apply time, see SaveArtifactScope callers)
// links this runtime identity back to the exact kind: sdk/kind: mcp document
// that created it, and that document's own webhook_attachment field
// (sdkConfigDocument.WebhookAttachment) names the registration whose events
// this connection may receive -- see plans/plan-webhook-kind.md's NATS/WS
// section. A missing scope, a scope with no config_key (never happens for a
// config-created scope, but defensive), or a config with no
// webhook_attachment all return ("", nil): "this connection gets no webhook
// subscription," not an error -- most SDKs/MCPs never attach a webhook at
// all, and that's a normal, valid connection.
func resolveWebhookAttachmentLabel(ctx context.Context, configStore store.ConfigRepository, s store.Store, artifactID uuid.UUID) (string, error) {
	scope, err := s.GetArtifactScope(ctx, artifactID)
	if err != nil {
		if errors.Is(err, store.ErrArtifactScopeNotFound) {
			return "", nil
		}
		return "", err
	}
	if strings.TrimSpace(scope.ConfigKey) == "" {
		return "", nil
	}
	state, err := configStore.GetConfigState(ctx, scope.ConfigKey)
	if err != nil {
		return "", err
	}
	if state == nil {
		return "", nil
	}
	var doc struct {
		WebhookAttachment string `json:"webhook_attachment"`
	}
	if err := json.Unmarshal(state.DesiredState, &doc); err != nil {
		return "", err
	}
	return strings.TrimSpace(doc.WebhookAttachment), nil
}

// subjectSafeLabel guards the NATS subject's fixed segment positions -- see
// sandbox/webhook.go's identical function (duplicated rather than shared
// across packages for one line) for why a literal "." in a label must never
// reach the subject as-is.
func subjectSafeLabel(label string) string {
	return strings.ReplaceAll(label, ".", "-")
}

func computeLatencyMs(startTimeStr string) int64 {
	if startTimeStr == "" {
		return 0
	}
	start, err := time.Parse(time.RFC3339Nano, startTimeStr)
	if err != nil {
		return 0
	}
	return time.Since(start).Milliseconds()
}
