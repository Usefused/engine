package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const WebhookDiagnosticsRetention = 7 * 24 * time.Hour

// WebhookDiagnosticScope binds private evidence to the exact canonical receipt, never an event name or route prefix.
type WebhookDiagnosticScope struct {
	AccountID, ServiceID, RegistrationID, EventID uuid.UUID
}

// WebhookDiagnosticsStore deliberately keeps message bodies outside the canonical receipt and cache interfaces.
type WebhookDiagnosticsStore interface {
	SaveWebhookDiagnostics(context.Context, WebhookDiagnosticScope, json.RawMessage, []byte) error
	GetWebhookDiagnostics(context.Context, WebhookDiagnosticScope, []byte) (json.RawMessage, error)
	DeleteExpiredWebhookDiagnostics(context.Context, time.Time, int) (int64, error)
}

// SaveWebhookDiagnostics can precede asynchronous receipt persistence; evidence is encrypted and immutable.
func (s *postgresStore) SaveWebhookDiagnostics(ctx context.Context, scope WebhookDiagnosticScope, payload json.RawMessage, key []byte) error {
	// Missing identities or oversized evidence must never create a loosely scoped private record.
	if scope.AccountID == uuid.Nil || scope.ServiceID == uuid.Nil || scope.RegistrationID == uuid.Nil || scope.EventID == uuid.Nil || !json.Valid(payload) || len(payload) > 512<<10 {
		return ErrExecutionResultInvalid
	}
	wrapped, ciphertext, err := sealReplayEvidence(payload, key)
	// Encryption errors cannot fall back to plaintext capture.
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO fused_webhook_diagnostics (event_id,account_id,service_id,registration_id,diagnostic_dek,diagnostic_payload,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,NOW()+$7*INTERVAL '1 second') ON CONFLICT (event_id) DO NOTHING`, scope.EventID, scope.AccountID, scope.ServiceID, scope.RegistrationID, wrapped, ciphertext, int64(WebhookDiagnosticsRetention/time.Second))
	// Neither SQL failure nor a duplicate capture may replace original evidence.
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrExecutionResultNotFound
	}
	return nil
}

// GetWebhookDiagnostics checks both private scope and retained canonical receipt before decrypting anything.
func (s *postgresStore) GetWebhookDiagnostics(ctx context.Context, scope WebhookDiagnosticScope, key []byte) (json.RawMessage, error) {
	var wrapped, ciphertext string
	err := s.db.QueryRow(ctx, `SELECT d.diagnostic_dek,d.diagnostic_payload FROM fused_webhook_diagnostics d
		JOIN fused_engine_execution_events e ON e.id=d.event_id AND e.account_id=d.account_id AND e.service_id=d.service_id AND e.webhook_id=d.registration_id AND e.transport='webhook'
		WHERE d.event_id=$1 AND d.account_id=$2 AND d.service_id=$3 AND d.registration_id=$4 AND d.expires_at>NOW()`, scope.EventID, scope.AccountID, scope.ServiceID, scope.RegistrationID).Scan(&wrapped, &ciphertext)
	// Unknown, expired, deleted, and cross-scope receipts have the same non-disclosing outcome.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionResultNotFound
	}
	// A failed read must not attempt decryption with incomplete fields.
	if err != nil {
		return nil, err
	}
	return openReplayEvidence(wrapped, ciphertext, key)
}

// DeleteExpiredWebhookDiagnostics bounds cleanup and honors shorter canonical receipt retention too.
func (s *postgresStore) DeleteExpiredWebhookDiagnostics(ctx context.Context, receiptCutoff time.Time, limit int) (int64, error) {
	result, err := s.db.Exec(ctx, `WITH expired AS (
		SELECT event_id FROM fused_webhook_diagnostics WHERE expires_at<=NOW() OR created_at<$1 ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED
	) DELETE FROM fused_webhook_diagnostics d USING expired WHERE d.event_id=expired.event_id`, receiptCutoff, limit)
	// Cleanup errors remain bounded operational failures, never plaintext logs.
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

// SaveWebhookDiagnostics bypasses runtime caches so private evidence remains encrypted and write-once.
func (s *cachedStore) SaveWebhookDiagnostics(ctx context.Context, scope WebhookDiagnosticScope, payload json.RawMessage, key []byte) error {
	repository, ok := s.Store.(WebhookDiagnosticsStore)
	// Unsupported stores cannot silently retain a plaintext fallback.
	if !ok {
		return ErrExecutionResultNotFound
	}
	return repository.SaveWebhookDiagnostics(ctx, scope, payload, key)
}

// GetWebhookDiagnostics never caches decrypted content across actors or requests.
func (s *cachedStore) GetWebhookDiagnostics(ctx context.Context, scope WebhookDiagnosticScope, key []byte) (json.RawMessage, error) {
	repository, ok := s.Store.(WebhookDiagnosticsStore)
	// Missing capability remains explicit for old or test adapters.
	if !ok {
		return nil, ErrExecutionResultNotFound
	}
	return repository.GetWebhookDiagnostics(ctx, scope, key)
}

// DeleteExpiredWebhookDiagnostics delegates only to the private retention capability.
func (s *cachedStore) DeleteExpiredWebhookDiagnostics(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	repository, ok := s.Store.(WebhookDiagnosticsStore)
	// Stores without captured bodies have nothing private to clean up.
	if !ok {
		return 0, nil
	}
	return repository.DeleteExpiredWebhookDiagnostics(ctx, cutoff, limit)
}
