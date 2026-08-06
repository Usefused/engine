package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// StartMCPSessionWorker keeps connection lifecycle state separate from call
// history. Tool executions use the canonical execution-event subject; this
// worker only owns session start/end rows needed by the live-agent view.
func StartMCPSessionWorker(ctx context.Context, engineStore store.Store, natsClient *messaging.NATSClient) {
	if natsClient == nil || natsClient.JS == nil {
		slog.WarnContext(ctx, "MCP session worker not started: JetStream client is unavailable")
		return
	}
	_, err := natsClient.JS.QueueSubscribe(messaging.FusedEngineSessionWildcard, "fused_engine_session_workers", func(message *nats.Msg) {
		persistMCPSessionMessage(ctx, engineStore, message)
	}, nats.ManualAck())
	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to MCP session events", slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "Started MCP session consumer")
}

func persistMCPSessionMessage(ctx context.Context, engineStore store.Store, message *nats.Msg) {
	thread, _ := observability.Start(ctx, "Worker: MCP Session", "", "worker:mcp_session")
	defer thread.Complete(ctx, "Processed")
	session, err := decodeMCPSession(message.Data, time.Now())
	if err != nil {
		slog.WarnContext(ctx, "Discarding invalid MCP session event", slog.Any("error", err))
		_ = message.Ack()
		return
	}
	if err := engineStore.UpsertMCPSession(ctx, &session); err != nil {
		slog.ErrorContext(ctx, "Failed to persist MCP session", slog.Any("error", err))
		_ = message.Nak()
		return
	}
	_ = message.Ack()
}

func decodeMCPSession(payload []byte, occurredAt time.Time) (models.MCPSession, error) {
	var data struct {
		AppID     string `json:"app_id"`
		SessionID string `json:"session_id"`
		Type      string `json:"type"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return models.MCPSession{}, err
	}
	appID, err := uuid.Parse(data.AppID)
	if err != nil {
		return models.MCPSession{}, err
	}
	session := models.MCPSession{
		ID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(appID.String()+":"+data.SessionID)),
		AppID: appID, SessionID: data.SessionID,
	}
	if data.Type == "started" {
		session.StartedAt = occurredAt
	}
	if data.Type == "ended" {
		session.EndedAt = &occurredAt
	}
	return session, nil
}
