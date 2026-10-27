package store

import (
	"context"
	"errors"
	"github.com/Usefused/engine/internal/shared/models"
)

// SaveBridgeSnapshotResponse reuses ordinary run-retained response storage. Bridge owns the durable dispatch fence.
// Concurrent duplicate replies may acknowledge the same bytes, but never replace a different request or response.
func (s *postgresStore) SaveBridgeSnapshotResponse(ctx context.Context, r *models.IdempotentExecution) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO fused_engine_idempotency_keys
 (id,app_id,idempotency_key_hash,request_body_hash,response_body,response_status,response_media_family,expires_at,pending,run_id)
 VALUES($1,$2,$3,$4,$5,200,'json',$6,false,$7)
 ON CONFLICT(app_id,idempotency_key_hash) DO UPDATE SET id=fused_engine_idempotency_keys.id
 WHERE NOT fused_engine_idempotency_keys.pending AND fused_engine_idempotency_keys.request_body_hash=EXCLUDED.request_body_hash
 AND fused_engine_idempotency_keys.response_body=EXCLUDED.response_body AND fused_engine_idempotency_keys.run_id=EXCLUDED.run_id`, r.ID, r.AppID, r.IdempotencyKeyHash, r.RequestBodyHash, r.ResponseBody, r.ExpiresAt, r.RunID)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrIdempotencyKeyConflict
	}
	return err
}

// SaveBridgeSnapshotResponse keeps cache wrappers on the same durable result boundary.
func (s *cachedStore) SaveBridgeSnapshotResponse(ctx context.Context, r *models.IdempotentExecution) error {
	target, ok := s.Store.(interface {
		SaveBridgeSnapshotResponse(context.Context, *models.IdempotentExecution) error
	})
	if !ok {
		return errors.New("Bridge snapshot storage unavailable")
	}
	return target.SaveBridgeSnapshotResponse(ctx, r)
}
