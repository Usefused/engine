package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
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

type mcpSessionEventData struct {
	AppID           string    `json:"app_id"`
	AppTokenID      string    `json:"app_token_id"`
	SessionID       string    `json:"session_id"`
	ProtocolVersion string    `json:"protocol_version"`
	Type            string    `json:"type"`
	EndReason       string    `json:"end_reason"`
	Timestamp       time.Time `json:"timestamp"`
	LastActivityAt  time.Time `json:"last_activity_at"`
}

func decodeMCPSession(payload []byte, occurredAt time.Time) (models.MCPSession, error) {
	data, err := decodeMCPSessionEventData(payload)
	if err != nil {
		return models.MCPSession{}, err
	}
	appID, tokenID, err := parseMCPSessionEventIDs(data)
	if err != nil {
		return models.MCPSession{}, err
	}
	if !validMCPSessionEventData(data) {
		return models.MCPSession{}, errors.New("mcp session event is invalid")
	}
	recordedAt := occurredAt
	if !data.Timestamp.IsZero() {
		// Persist producer time so JetStream delivery delay does not distort
		// session duration or token-use history after a worker restart.
		recordedAt = data.Timestamp.UTC()
	}
	protocolVersion, err := normalizedMCPProtocolVersion(data.ProtocolVersion)
	if err != nil {
		return models.MCPSession{}, err
	}
	lastActivityAt := recordedAt
	if !data.LastActivityAt.IsZero() {
		lastActivityAt = data.LastActivityAt.UTC()
		if lastActivityAt.After(recordedAt) {
			return models.MCPSession{}, errors.New("mcp session activity is after the event")
		}
	}
	session := models.MCPSession{
		ID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(appID.String()+":"+data.SessionID)),
		AppID: appID, AppTokenID: tokenID, SessionID: data.SessionID,
		ProtocolVersion: protocolVersion,
		StartedAt:       recordedAt, LastActivityAt: lastActivityAt,
	}
	if data.Type == "ended" {
		session.EndedAt = &recordedAt
		session.EndReason = data.EndReason
	}
	return session, nil
}

func decodeMCPSessionEventData(payload []byte) (mcpSessionEventData, error) {
	var data mcpSessionEventData
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return mcpSessionEventData{}, err
	}
	// Strict single-document decoding keeps the durable session history from
	// accepting producer drift that operators could not reliably interpret.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return mcpSessionEventData{}, errors.New("mcp session event must contain one document")
	}
	return data, nil
}

func parseMCPSessionEventIDs(data mcpSessionEventData) (uuid.UUID, uuid.UUID, error) {
	appID, err := uuid.Parse(data.AppID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	tokenID, err := optionalMCPSessionUUID(data.AppTokenID)
	return appID, tokenID, err
}

func validMCPSessionEventData(data mcpSessionEventData) bool {
	if data.SessionID == "" {
		return false
	}
	if data.Type == "started" {
		return data.EndReason == ""
	}
	return data.Type == "ended" && validMCPSessionEndReason(data.EndReason)
}

func validMCPSessionEndReason(reason string) bool {
	// Keep producer validation aligned with the database constraint so a bad
	// event is acknowledged once instead of being NAKed into an endless retry.
	switch reason {
	case "client_terminated", "client_disconnected", "idle_timeout", "token_expired",
		"token_revoked", "app_deactivated", "engine_shutdown", "runtime_failed", "tool_call_timeout":
		return true
	default:
		return false
	}
}

func normalizedMCPProtocolVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "2024-11-05", nil
	}
	if len(version) > 32 || strings.ContainsAny(version, " \t\r\n") {
		return "", errors.New("mcp protocol version is invalid")
	}
	return version, nil
}

func optionalMCPSessionUUID(value string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(value)
}
