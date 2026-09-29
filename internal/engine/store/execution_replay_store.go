package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
)

const MaxReplayEvidenceBytes = canonicaljson.MaxInputBytes

var (
	ErrReplayEvidenceNotFound = errors.New("replay evidence not found")
	ErrReplayEvidenceInvalid  = errors.New("replay evidence is invalid")
)

// ExecutionReplayEvidenceStore persists one encrypted, immutable call history for a terminal execution.
type ExecutionReplayEvidenceStore interface {
	SaveReplayEvidence(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, json.RawMessage, []byte) error
	GetReplayEvidence(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte) (json.RawMessage, error)
	DeleteExpiredReplayEvidence(context.Context, time.Time, int) (int64, error)
}

// sealReplayEvidence encrypts a bounded canonical history with a fresh per-record data key.
func sealReplayEvidence(history json.RawMessage, masterKey []byte) (string, string, error) {
	// Empty or malformed recordings cannot establish a deterministic replay contract.
	if len(history) == 0 || len(history) > MaxReplayEvidenceBytes {
		return "", "", ErrReplayEvidenceInvalid
	}
	canonical, err := canonicaljson.Canonicalize(history)
	if err != nil {
		return "", "", ErrReplayEvidenceInvalid
	}
	wrapped, dek, err := WrapDEK(masterKey)
	if err != nil {
		return "", "", err
	}
	ciphertext, err := EncryptWithDEK(dek, string(canonical))
	if err != nil {
		return "", "", err
	}
	return wrapped, ciphertext, nil
}

// openReplayEvidence unwraps only a matching Engine key and returns validated JSON.
func openReplayEvidence(wrapped, ciphertext string, masterKey []byte) (json.RawMessage, error) {
	dek, err := UnwrapDEK(masterKey, wrapped)
	if err != nil {
		return nil, err
	}
	plaintext, err := DecryptWithDEK(dek, ciphertext)
	if err != nil {
		return nil, err
	}
	canonical, err := canonicaljson.Canonicalize([]byte(plaintext))
	if err != nil {
		return nil, ErrReplayEvidenceInvalid
	}
	return canonical, nil
}

// SaveReplayEvidence commits only after the matching execution reaches a terminal state.
func (s *postgresStore) SaveReplayEvidence(ctx context.Context, accountID, appID, executionID uuid.UUID, history json.RawMessage, masterKey []byte) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_replay_evidence.save")
	defer span.End()
	wrapped, ciphertext, err := sealReplayEvidence(history, masterKey)
	if err != nil {
		return err
	}
	// The SELECT binds evidence to one unexpired terminal result and enforces the same minimum window.
	tag, err := s.db.Exec(ctx, `INSERT INTO fused_unified_app_replay_evidence
		(execution_id, account_id, app_id, encrypted_dek, encrypted_history, expires_at)
		SELECT id, account_id, app_id, $4, $5, GREATEST(expires_at, NOW()+INTERVAL '24 hours')
		FROM fused_unified_app_results
		WHERE id=$1 AND account_id=$2 AND app_id=$3 AND status IN ('succeeded','failed','indeterminate')
			AND expires_at>NOW() AND mode<>'replay'
		ON CONFLICT (execution_id) DO NOTHING`, executionID, accountID, appID, wrapped, ciphertext)
	if err != nil {
		return err
	}
	// A duplicate or nonterminal result cannot silently replace replay evidence.
	if tag.RowsAffected() != 1 {
		return ErrReplayEvidenceInvalid
	}
	return nil
}

// GetReplayEvidence reads one retained encrypted history under exact tenant and app scope.
func (s *postgresStore) GetReplayEvidence(ctx context.Context, accountID, appID, executionID uuid.UUID, masterKey []byte) (json.RawMessage, error) {
	var wrapped, ciphertext string
	err := s.db.QueryRow(ctx, `SELECT evidence.encrypted_dek, evidence.encrypted_history
		FROM fused_unified_app_replay_evidence evidence
		JOIN fused_unified_app_results result ON result.id=evidence.execution_id
		WHERE evidence.execution_id=$1 AND evidence.account_id=$2 AND evidence.app_id=$3
			AND result.account_id=$2 AND result.app_id=$3
			AND evidence.expires_at>NOW() AND result.expires_at>NOW()`, executionID, accountID, appID).
		Scan(&wrapped, &ciphertext)
	// Expired, missing, and cross-tenant recordings share one unavailable result.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReplayEvidenceNotFound
	}
	if err != nil {
		return nil, err
	}
	return openReplayEvidence(wrapped, ciphertext, masterKey)
}

// DeleteExpiredReplayEvidence removes bounded expired rows without touching retained results.
func (s *postgresStore) DeleteExpiredReplayEvidence(ctx context.Context, before time.Time, limit int) (int64, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_replay_evidence.expire")
	defer span.End()
	// A fixed batch cap protects live execution traffic during cleanup.
	if limit < 1 || limit > 1000 {
		return 0, ErrReplayEvidenceInvalid
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_unified_app_replay_evidence WHERE execution_id IN (
		SELECT execution_id FROM fused_unified_app_replay_evidence
		WHERE expires_at<LEAST($1,NOW()) ORDER BY expires_at, execution_id LIMIT $2
	)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// replayEvidenceDelegate preserves optional store compatibility without caching decrypted histories.
func (s *cachedStore) replayEvidenceDelegate() (ExecutionReplayEvidenceStore, error) {
	repository, ok := s.Store.(ExecutionReplayEvidenceStore)
	if !ok {
		return nil, errors.New("replay evidence store unavailable")
	}
	return repository, nil
}

// SaveReplayEvidence delegates encryption and durable admission to the underlying store.
func (s *cachedStore) SaveReplayEvidence(ctx context.Context, accountID, appID, executionID uuid.UUID, history json.RawMessage, masterKey []byte) error {
	repository, err := s.replayEvidenceDelegate()
	if err != nil {
		return err
	}
	return repository.SaveReplayEvidence(ctx, accountID, appID, executionID, history, masterKey)
}

// GetReplayEvidence delegates exact reads without caching plaintext history.
func (s *cachedStore) GetReplayEvidence(ctx context.Context, accountID, appID, executionID uuid.UUID, masterKey []byte) (json.RawMessage, error) {
	repository, err := s.replayEvidenceDelegate()
	if err != nil {
		return nil, err
	}
	return repository.GetReplayEvidence(ctx, accountID, appID, executionID, masterKey)
}

// DeleteExpiredReplayEvidence delegates bounded encrypted-history cleanup.
func (s *cachedStore) DeleteExpiredReplayEvidence(ctx context.Context, before time.Time, limit int) (int64, error) {
	repository, err := s.replayEvidenceDelegate()
	if err != nil {
		return 0, err
	}
	return repository.DeleteExpiredReplayEvidence(ctx, before, limit)
}

var _ ExecutionReplayEvidenceStore = (*postgresStore)(nil)
var _ ExecutionReplayEvidenceStore = (*cachedStore)(nil)
