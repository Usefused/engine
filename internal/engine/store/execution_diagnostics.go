package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExecutionDiagnosticsStore separates privileged payload retrieval from ordinary result and audit projections.
type ExecutionDiagnosticsStore interface {
	SaveExecutionDiagnostics(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, json.RawMessage, []byte) error
	GetExecutionDiagnostics(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte) (json.RawMessage, error)
}

// SaveExecutionDiagnostics encrypts bounded details on the same retained terminal result.
func (s *postgresStore) SaveExecutionDiagnostics(ctx context.Context, accountID, appID, id uuid.UUID, payload json.RawMessage, key []byte) error {
	// Invalid payloads cannot consume unbounded storage or overwrite existing diagnostics.
	if !json.Valid(payload) || len(payload) > 1<<20 {
		return ErrExecutionResultInvalid
	}
	wrapped, ciphertext, err := sealReplayEvidence(payload, key)
	// Encryption failure must never fall back to plaintext retention.
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE fused_unified_app_results SET diagnostic_dek=$4,diagnostic_payload=$5 WHERE id=$1 AND account_id=$2 AND app_id=$3 AND status IN ('succeeded','failed','indeterminate') AND expires_at>NOW() AND diagnostic_payload IS NULL`, id, accountID, appID, wrapped, ciphertext)
	// A failed SQL write leaves the already-completed execution untouched.
	if err != nil {
		return err
	}
	// The scope, retention window, terminal state, and write-once guard must all match.
	if tag.RowsAffected() != 1 {
		return ErrExecutionResultNotFound
	}
	return nil
}

// GetExecutionDiagnostics never projects private data without exact account, version, and retention checks.
func (s *postgresStore) GetExecutionDiagnostics(ctx context.Context, accountID, appID, id uuid.UUID, key []byte) (json.RawMessage, error) {
	var wrapped, ciphertext string
	err := s.db.QueryRow(ctx, `SELECT diagnostic_dek,diagnostic_payload FROM fused_unified_app_results WHERE id=$1 AND account_id=$2 AND app_id=$3 AND expires_at>NOW() AND diagnostic_payload IS NOT NULL`, id, accountID, appID).Scan(&wrapped, &ciphertext)
	// Missing and expired records share one response and cannot reveal another account's execution.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionResultNotFound
	}
	// Storage errors cannot fall through into decryption of incomplete fields.
	if err != nil {
		return nil, err
	}
	return openReplayEvidence(wrapped, ciphertext, key)
}

// SaveExecutionDiagnostics bypasses caches because these bytes must remain write-once and encrypted.
func (s *cachedStore) SaveExecutionDiagnostics(ctx context.Context, accountID, appID, id uuid.UUID, payload json.RawMessage, key []byte) error {
	repository, ok := s.Store.(ExecutionDiagnosticsStore)
	// Optional backing stores cannot manufacture missing private evidence.
	if !ok {
		return ErrExecutionResultNotFound
	}
	return repository.SaveExecutionDiagnostics(ctx, accountID, appID, id, payload, key)
}

// GetExecutionDiagnostics never caches privileged decrypted content across actors.
func (s *cachedStore) GetExecutionDiagnostics(ctx context.Context, accountID, appID, id uuid.UUID, key []byte) (json.RawMessage, error) {
	repository, ok := s.Store.(ExecutionDiagnosticsStore)
	// Optional backing stores cannot manufacture missing private evidence.
	if !ok {
		return nil, ErrExecutionResultNotFound
	}
	return repository.GetExecutionDiagnostics(ctx, accountID, appID, id, key)
}
