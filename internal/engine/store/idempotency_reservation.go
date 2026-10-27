package store

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

var ErrIdempotencyInProgress = errors.New("idempotency outcome pending or unknown; provider was not recalled")

// Run-bound responses survive old attempt expiry while the current attempt remains retained.
const retainedIdempotentExecution = `((k.run_id IS NULL AND (k.pending OR k.expires_at>NOW())) OR EXISTS (
 SELECT 1 FROM fused_unified_app_recovery c JOIN fused_unified_app_results r ON r.id=c.execution_id
 WHERE c.run_id=k.run_id AND c.current AND (r.expires_at IS NULL OR r.expires_at>NOW())) OR EXISTS (
 SELECT 1 FROM fused_engine_idempotency_runs d
 JOIN fused_unified_app_recovery c ON c.run_id=d.run_id AND c.current
 JOIN fused_unified_app_results r ON r.id=c.execution_id
 WHERE d.idempotency_id=k.id AND (r.expires_at IS NULL OR r.expires_at>NOW())))`

// ReserveIdempotentExecution uses the unique app/key constraint as the cross-process dispatch fence.
func (s *postgresStore) ReserveIdempotentExecution(ctx context.Context, execution *models.IdempotentExecution) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO fused_engine_idempotency_keys
 (id,app_id,idempotency_key_hash,request_body_hash,expires_at,pending,run_id)
 VALUES($1,$2,$3,$4,$5,true,$6)
 ON CONFLICT(app_id,idempotency_key_hash) DO UPDATE SET id=EXCLUDED.id,request_body_hash=EXCLUDED.request_body_hash,
 expires_at=EXCLUDED.expires_at,pending=true,run_id=EXCLUDED.run_id,response_body=NULL
 WHERE NOT fused_engine_idempotency_keys.pending AND fused_engine_idempotency_keys.run_id IS NULL AND fused_engine_idempotency_keys.expires_at<=NOW()
 AND NOT EXISTS(SELECT 1 FROM fused_engine_idempotency_runs d WHERE d.idempotency_id=fused_engine_idempotency_keys.id)`, execution.ID, execution.AppID, execution.IdempotencyKeyHash, execution.RequestBodyHash, execution.ExpiresAt, execution.RunID)
	// Only the insert/update winner may enter the provider dispatcher.
	if err == nil && tag.RowsAffected() != 1 {
		return ErrIdempotencyInProgress
	}
	return err
}

// ReleaseIdempotentExecution permits retry only when the dispatching owner knows repetition is safe.
func (s *postgresStore) ReleaseIdempotentExecution(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM fused_engine_idempotency_keys WHERE id=$1 AND pending`, id)
	return err
}

// expireIdempotentExecutions shares bounded result maintenance and never expires uncertain standalone writes.
func (s *postgresStore) expireIdempotentExecutions(ctx context.Context, limit int) error {
	// Remove expired dependencies before deciding whether their shared response is still retained.
	if err := s.expireIdempotentRunReferences(ctx, limit); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM fused_engine_idempotency_keys WHERE id IN (
 SELECT k.id FROM fused_engine_idempotency_keys k WHERE NOT `+retainedIdempotentExecution+` LIMIT $1)`, limit)
	return err
}

// RetainIdempotentExecution records a dependency before exposing shared bytes to another active run.
func (s *postgresStore) RetainIdempotentExecution(ctx context.Context, id, run uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO fused_engine_idempotency_runs(idempotency_id,run_id)
 SELECT k.id,c.run_id FROM fused_engine_idempotency_keys k
 JOIN fused_unified_app_recovery c ON c.app_id=k.app_id AND c.run_id=$2 AND c.current
 JOIN fused_unified_app_results r ON r.id=c.execution_id
 WHERE k.id=$1 AND NOT k.pending AND r.status IN ('queued','running') AND (r.expires_at IS NULL OR r.expires_at>NOW())
 ON CONFLICT(idempotency_id,run_id) DO UPDATE SET run_id=EXCLUDED.run_id`, id, run)
	// An expired or superseded run cannot acknowledge a new replay dependency.
	if err == nil && tag.RowsAffected() != 1 {
		return ErrExecutionRecoveryConflict
	}
	return err
}

// expireIdempotentRunReferences bounds metadata growth for business keys reused by many successive runs.
func (s *postgresStore) expireIdempotentRunReferences(ctx context.Context, limit int) error {
	_, err := s.db.Exec(ctx, `DELETE FROM fused_engine_idempotency_runs WHERE (idempotency_id,run_id) IN (
 SELECT d.idempotency_id,d.run_id FROM fused_engine_idempotency_runs d WHERE NOT EXISTS (
 SELECT 1 FROM fused_unified_app_recovery c JOIN fused_unified_app_results r ON r.id=c.execution_id
 WHERE c.run_id=d.run_id AND c.current AND (r.expires_at IS NULL OR r.expires_at>NOW())) LIMIT $1)`, limit)
	return err
}
