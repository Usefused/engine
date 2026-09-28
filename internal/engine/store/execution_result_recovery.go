package store

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
)

// ExecutionResultRecoveryStore marks abandoned accepted work without rerunning provider effects.
type ExecutionResultRecoveryStore interface {
	RecoverStaleExecutionResults(context.Context, time.Time, int) (int64, error)
}

// RecoverStaleExecutionResults gives abandoned rows a terminal result in one bounded SQL update.
func (s *postgresStore) RecoverStaleExecutionResults(ctx context.Context, before time.Time, limit int) (int64, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_result.recover_stale")
	defer span.End()
	// A fixed batch bound protects live execution writes from maintenance work.
	if limit < 1 || limit > 1000 {
		return 0, ErrExecutionResultInvalid
	}
	tag, err := s.db.Exec(ctx, `WITH abandoned AS (
		SELECT id FROM fused_execution_app_results
		WHERE status IN ('queued','running') AND updated_at<$1
		ORDER BY updated_at, id LIMIT $2 FOR UPDATE SKIP LOCKED
	)
	UPDATE fused_execution_app_results result SET
		status=CASE WHEN result.status='running' THEN 'indeterminate' ELSE 'failed' END,
		error_code=CASE WHEN result.status='running' THEN 'execution_interrupted' ELSE 'runtime_unavailable' END,
		error_message='capability execution did not complete successfully',
		updated_at=NOW(), completed_at=NOW(), expires_at=NOW()+INTERVAL '24 hours'
	FROM abandoned WHERE result.id=abandoned.id`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RecoverStaleExecutionResults delegates recovery only to stores with durable execution rows.
func (s *cachedStore) RecoverStaleExecutionResults(ctx context.Context, before time.Time, limit int) (int64, error) {
	repository, ok := s.Store.(ExecutionResultRecoveryStore)
	// A cache-only store cannot infer which provider effects might already have happened.
	if !ok {
		return 0, errors.New("execution result recovery is unavailable")
	}
	return repository.RecoverStaleExecutionResults(ctx, before, limit)
}

var _ ExecutionResultRecoveryStore = (*postgresStore)(nil)
var _ ExecutionResultRecoveryStore = (*cachedStore)(nil)
