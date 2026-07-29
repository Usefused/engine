package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
)

// StartMCPAnalyticsWorker starts consumers for Fused Engine runtime events.
func StartMCPAnalyticsWorker(ctx context.Context, engineStore store.Store, natsClient *messaging.NATSClient, cfg *config.Config) {
	if natsClient == nil || natsClient.JS == nil {
		slog.WarnContext(ctx, "MCP Analytics Worker not started: NATS JetStream client is nil")
		return
	}

	// Process analytics
	_, err := natsClient.JS.QueueSubscribe(messaging.FusedEngineAnalyticsWildcard, "fused_engine_analytics_workers", func(msg *nats.Msg) {
		thread, _ := observability.Start(ctx, "Worker: MCP Analytics", "", "worker:mcp_analytics")
		defer thread.Complete(ctx, "Processed")

		var data map[string]any
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal fused_engine.analytics event", slog.Any("error", err))
			thread.Step("Failed to unmarshal fused_engine.analytics event").Error(ctx, err)
			msg.Ack()
			return
		}

		// Extract execution metadata. Params and result are captured because the
		// user owns the MCP executor \u2014 credentials are stripped at publish time.
		artifactIDStr, _ := data["artifact_id"].(string)
		sessionID, _ := data["session_id"].(string)
		endpointName, _ := data["endpoint_name"].(string)
		serviceName, _ := data["service_name"].(string)
		latencyMs, _ := data["latency_ms"].(float64)
		failed, _ := data["failed"].(bool)

		// params and result are raw JSON — re-encode as bytes for storage.
		// sanitiseParams was already applied by the publisher; we store as-is.
		var paramsJSON, resultJSON []byte
		if p, ok := data["params"]; ok && p != nil {
			paramsJSON, _ = json.Marshal(p)
		}
		if r, ok := data["result"]; ok && r != nil {
			resultJSON, _ = json.Marshal(r)
		}

		artifactID, err := uuid.Parse(artifactIDStr)
		if err != nil {
			msg.Ack()
			return
		}

		analytics := &models.MCPAnalytics{
			ID:           uuid.New(),
			ArtifactID:   artifactID,
			SessionID:    sessionID,
			EndpointName: endpointName,
			ServiceName:  serviceName,
			LatencyMs:    int64(latencyMs),
			Failed:       failed,
			Timestamp:    time.Now(),
			Params:       paramsJSON,
			Result:       resultJSON,
		}

		if err := engineStore.InsertMCPAnalytics(context.Background(), analytics); err != nil {
			slog.ErrorContext(ctx, "Failed to insert MCP analytics", slog.Any("error", err))
			thread.Step("Failed to insert MCP analytics").Error(ctx, err)
			msg.Nak()
			return
		}

		msg.Ack()
	}, nats.ManualAck())

	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to fused_engine.analytics.>", slog.Any("error", err))
	} else {
		slog.InfoContext(ctx, "Started MCP Analytics consumer")
	}

	// Process sessions
	_, err = natsClient.JS.QueueSubscribe(messaging.FusedEngineSessionWildcard, "fused_engine_session_workers", func(msg *nats.Msg) {
		thread, _ := observability.Start(ctx, "Worker: MCP Session", "", "worker:mcp_session")
		defer thread.Complete(ctx, "Processed")

		var data map[string]any
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal fused_engine.session event", slog.Any("error", err))
			thread.Step("Failed to unmarshal fused_engine.session event").Error(ctx, err)
			msg.Ack()
			return
		}

		artifactIDStr, _ := data["artifact_id"].(string)
		sessionID, _ := data["session_id"].(string)
		eventType, _ := data["type"].(string)

		artifactID, err := uuid.Parse(artifactIDStr)
		if err != nil {
			msg.Ack()
			return
		}

		now := time.Now()
		// Derive a stable UUID from artifact_id + session_id so that "started" and
		// "ended" events for the same logical session always resolve to the same
		// row, and the ON CONFLICT (id) upsert works correctly.
		sessionUUID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(artifactID.String()+":"+sessionID))
		session := &models.MCPSession{
			ID:         sessionUUID,
			ArtifactID: artifactID,
			SessionID:  sessionID,
		}

		if eventType == "started" {
			session.StartedAt = now
		} else if eventType == "ended" {
			session.EndedAt = &now
		}

		if err := engineStore.UpsertMCPSession(context.Background(), session); err != nil {
			slog.ErrorContext(ctx, "Failed to upsert MCP session", slog.Any("error", err))
			thread.Step("Failed to upsert MCP session").Error(ctx, err)
			msg.Nak()
			return
		}

		msg.Ack()
	}, nats.ManualAck())

	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to fused_engine.session.>", slog.Any("error", err))
	} else {
		slog.InfoContext(ctx, "Started MCP Session consumer")
	}
}
