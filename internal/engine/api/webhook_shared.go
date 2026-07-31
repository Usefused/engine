package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// This file used to also hold the wss://.../sdks/ws WebSocket transport
// (SDKWebSocketHandler and its helpers). That path has been retired in favor
// of EngineGRPCServer.SubscribeWebhooks (webhook_grpc_handler.go), which
// rides the same gRPC channel every generated SDK already opens for
// Execute. The functions below are transport-agnostic -- durability/retry/
// queue-group fan-out logic and auth/label/filter helpers -- and are shared
// by the gRPC handler unchanged.

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
