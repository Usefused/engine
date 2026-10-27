package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReserveExecutionAttempt serializes manual and automatic triggers on the same logical run.
func (s *postgresStore) ReserveExecutionAttempt(ctx context.Context, request ExecutionRerunRequest) (*ExecutionResult, bool, error) {
	// Only bounded digests can establish a retryable trigger identity.
	if !validExecutionReadHash(request.KeyHash) {
		return nil, false, ErrExecutionResultInvalid
	}
	tx, err := s.db.Begin(ctx)
	// The old owner and replacement must change atomically across Engine replicas.
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	writer := &postgresStore{db: workspaceTransaction{Tx: tx}, executionResultRetentionDays: s.executionResultRetentionDays}
	current, err := scanRecovery(tx.QueryRow(ctx, `SELECT `+recoveryColumns+` FROM fused_unified_app_recovery WHERE current AND account_id=$1 AND app_id=$2 AND run_id=(SELECT run_id FROM fused_unified_app_recovery WHERE execution_id=$3 AND account_id=$1 AND app_id=$2) FOR UPDATE`, request.AccountID, request.AppID, request.SourceID))
	// Missing metadata and races cannot manufacture another logical run.
	if err != nil {
		return nil, false, ErrExecutionRecoveryConflict
	}
	existing, err := writer.findAttemptRequest(ctx, request)
	// A repeated request returns its original accepted attempt, including after later attempts exist.
	if err != nil || existing != nil {
		return existing, false, err
	}
	record, err := writer.lockRecoverableResult(ctx, request, *current)
	// Authorization, status, bounds and automatic delay are rechecked after locking ownership.
	if err != nil {
		return nil, false, err
	}
	next, err := writer.replaceExecutionAttempt(ctx, request, *current, *record)
	// An unknown write outcome must not dispatch a replacement worker.
	if err != nil {
		return nil, false, err
	}
	// Commit is the sole point at which the new process may acquire execution authority.
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	next.RunID = &current.RunID
	next.Attempt = current.Attempt + 1
	return next, true, nil
}

// findAttemptRequest deduplicates a caller key within its original source and token scope.
func (s *postgresStore) findAttemptRequest(ctx context.Context, request ExecutionRerunRequest) (*ExecutionResult, error) {
	var id, runID uuid.UUID
	var attempt int
	err := s.db.QueryRow(ctx, `SELECT execution_id,run_id,attempt FROM fused_unified_app_recovery WHERE account_id=$1 AND app_id=$2 AND request_source=$3 AND request_token=$4 AND request_key=$5`, request.AccountID, request.AppID, request.SourceID, request.TokenID, request.KeyHash).Scan(&id, &runID, &attempt)
	// Absence permits a fresh reservation; every other storage outcome remains an error.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	} // Database failure cannot be interpreted as absence.
	record, err := s.GetExecutionResult(ctx, request.AccountID, request.AppID, id)
	// A failed retained-result read cannot fabricate an accepted idempotent response.
	if err != nil {
		return nil, err
	}
	record.RunID, record.Attempt = &runID, attempt
	return record, nil
}

// lockRecoverableResult prevents completion racing a replacement and rechecks live deployment authority.
func (s *postgresStore) lockRecoverableResult(ctx context.Context, request ExecutionRerunRequest, state ExecutionRecovery) (*ExecutionResult, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT r.id FROM fused_unified_app_results r
 JOIN fused_unified_app_recovery c ON c.execution_id=r.id
 JOIN fused_app_families f ON f.app_family_id=r.app_family_id AND f.account_id=r.account_id
 JOIN fused_apps a ON a.app_id=r.app_id
 WHERE r.id=$1 AND r.account_id=$2 AND r.app_id=$3 AND (r.expires_at IS NULL OR r.expires_at>NOW())
 AND f.archived_at IS NULL AND f.unified_active_app_id=r.app_id AND a.status IN ('active','deprecated')
 AND c.attempt<c.max_attempts AND r.status<>'succeeded'
 AND (r.status IN ('failed','indeterminate') OR c.lease_until<=clock_timestamp())
 AND (NOT $4 OR (c.mode='automatic' AND (r.completed_at IS NULL OR r.completed_at+make_interval(secs=>c.backoff_seconds)<=NOW())))
 AND (($5::uuid=r.app_token_id AND `+recoveryOriginAuthority+`) OR (NOT $4 AND EXISTS(SELECT 1 FROM fused_app_tokens t WHERE t.id=$5 AND t.app_family_id=r.app_family_id AND (t.expires_at IS NULL OR t.expires_at>NOW()))))
 FOR UPDATE OF r`, state.ExecutionID, request.AccountID, request.AppID, request.Automatic, request.TokenID).Scan(&id)
	// Pending owners, exhausted budgets and changed deployment authority are explicit conflicts.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionRecoveryConflict
	}
	if err != nil {
		return nil, err
	} // Unknown database state cannot authorize a retry.
	return s.GetExecutionResult(ctx, request.AccountID, request.AppID, id)
}

// replaceExecutionAttempt retains the latest encrypted snapshot while assigning a fresh execution identity.
func (s *postgresStore) replaceExecutionAttempt(ctx context.Context, request ExecutionRerunRequest, state ExecutionRecovery, previous ExecutionResult) (*ExecutionResult, error) {
	// Interrupted owners retain their payloads for the operator-selected window, including no expiry.
	_, err := s.db.Exec(ctx, `UPDATE fused_unified_app_results SET status='indeterminate',error_code='execution_interrupted',error_message='Execution was interrupted',completed_at=NOW(),updated_at=NOW(),expires_at=CASE WHEN $2::int=0 THEN NULL ELSE NOW()+make_interval(days=>$2) END WHERE id=$1 AND status IN ('queued','running')`, previous.ID, s.resultRetentionDays())
	// Expired owners become terminal before a replacement result can be created.
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(ctx, `UPDATE fused_unified_app_recovery SET current=false WHERE execution_id=$1`, previous.ID)
	// The partial unique index permits the new owner only after releasing the old one in this transaction.
	if err != nil {
		return nil, err
	}
	next := ExecutionResult{ID: uuid.New(), AccountID: previous.AccountID, AppFamilyID: previous.AppFamilyID, AppID: previous.AppID, AppVersion: previous.AppVersion, AppTokenID: request.TokenID, ReadHandleHash: previous.ReadHandleHash, IdempotencyKeyHash: request.KeyHash, Status: "queued", Input: previous.Input, Mode: "rerun", SourceExecutionID: &previous.ID, SourceScheduleID: previous.SourceScheduleID, SourceWebhookEventID: previous.SourceWebhookEventID, BucketID: previous.BucketID, RecoveryOrigin: previous.RecoveryOrigin}
	// A fresh direct target token replaces delegation; automatic attempts retain their original chain.
	if request.TokenID != previous.AppTokenID {
		next.RecoveryOrigin = nil
	}
	// The shared result writer keeps ordinary history, retention and provider attribution intact.
	if err := s.CreateExecutionResult(ctx, next); err != nil {
		return nil, err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO fused_unified_app_recovery(execution_id,run_id,account_id,app_id,attempt,mode,max_attempts,backoff_seconds,revision,encrypted_dek,encrypted_snapshot,request_key,request_source,request_token,ready)
 SELECT $2,run_id,account_id,app_id,attempt+1,mode,max_attempts,backoff_seconds,revision,encrypted_dek,encrypted_snapshot,$3,$4,$5,$6 FROM fused_unified_app_recovery WHERE execution_id=$1`, previous.ID, next.ID, request.KeyHash, request.SourceID, request.TokenID, request.Enqueue)
	// Snapshot copying and attempt admission are indivisible; plaintext never enters SQL parameters.
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(ctx, `UPDATE fused_unified_app_schedules SET last_execution_id=$2,last_status='running',last_started_at=NOW(),revision=revision+1 WHERE last_execution_id=$1`, previous.ID, next.ID)
	// A retry does not advance cron or change a user's enabled/paused preference.
	if err != nil {
		return nil, err
	}
	return s.GetExecutionResult(ctx, next.AccountID, next.AppID, next.ID)
}
