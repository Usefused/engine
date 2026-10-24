package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const recoveryColumns = `execution_id,run_id,attempt,current,revision,lease_until,mode,max_attempts,backoff_seconds,ready`

// scanRecovery keeps public attempt metadata separate from encrypted continuation payloads.
func scanRecovery(row pgx.Row) (*ExecutionRecovery, error) {
	var state ExecutionRecovery
	err := row.Scan(&state.ExecutionID, &state.RunID, &state.Attempt, &state.Current, &state.Revision, &state.LeaseUntil, &state.Policy.Mode, &state.Policy.MaxAttempts, &state.Policy.BackoffSeconds, &state.Ready)
	return &state, err
}

// createRecoverableExecution atomically admits the result and its initial attempt fence.
func (s *postgresStore) createRecoverableExecution(ctx context.Context, record ExecutionResult) error {
	policy := *record.Recovery
	// Invalid policy cannot leave behind an accepted invocation without ownership.
	if err := policy.Validate(); err != nil {
		return err
	}
	// Autonomous recovery requires an origin whose current authority can be re-established without a bearer.
	if err := s.validateRecoveryOrigin(ctx, record); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	// Reservation requires both rows to commit or neither to exist.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	writer := &postgresStore{db: workspaceTransaction{Tx: tx}}
	record.Recovery = nil
	// The shared writer still owns input validation and ordinary result identity.
	if err := writer.CreateExecutionResult(ctx, record); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fused_unified_app_recovery(execution_id,run_id,account_id,app_id,attempt,mode,max_attempts,backoff_seconds) VALUES($1,$1,$2,$3,1,$4,$5,$6)`, record.ID, record.AccountID, record.AppID, policy.Mode, policy.MaxAttempts, policy.BackoffSeconds)
	// Never publish a result whose initial attempt could not be fenced.
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetExecutionRecovery returns only metadata scoped to the same retained result as ordinary reads.
func (s *postgresStore) GetExecutionRecovery(ctx context.Context, account, app, id uuid.UUID) (*ExecutionRecovery, error) {
	state, err := scanRecovery(s.db.QueryRow(ctx, `SELECT `+recoveryColumns+` FROM fused_unified_app_recovery WHERE execution_id=$1 AND account_id=$2 AND app_id=$3`, id, account, app))
	// Older executions and side-effect-free replays have no recovery metadata.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return state, err
}

// attemptAuthority requires live deployment and token authority even for a previously accepted invocation.
const attemptAuthority = `EXISTS (SELECT 1 FROM fused_unified_app_results r
 JOIN fused_app_families f ON f.app_family_id=r.app_family_id AND f.account_id=r.account_id
 JOIN fused_apps a ON a.app_id=r.app_id
 WHERE r.id=execution_id AND r.status IN ('queued','running') AND f.archived_at IS NULL
 AND f.unified_active_app_id=r.app_id AND a.status IN ('active','deprecated')
 AND (r.app_token_id='00000000-0000-0000-0000-000000000000' OR EXISTS
 (SELECT 1 FROM fused_app_tokens t WHERE t.id=r.app_token_id AND (t.expires_at IS NULL OR t.expires_at>clock_timestamp()))))`

// CheckExecutionAttempt fences host effects after supersession, expiry, revocation, or deployment changes.
func (s *postgresStore) CheckExecutionAttempt(ctx context.Context, account, app, id uuid.UUID) error {
	var allowed bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_unified_app_recovery WHERE execution_id=$1 AND account_id=$2 AND app_id=$3 AND current AND NOT ready AND lease_until>clock_timestamp() AND `+attemptAuthority+`)`, id, account, app).Scan(&allowed)
	// Uncertain authority must stop effects just like an explicitly lost lease.
	if err != nil {
		return err
	}
	// No process may continue under a replaced or expired attempt.
	if !allowed {
		return ErrExecutionRecoveryConflict
	}
	return nil
}

// ReadExecutionSnapshot decrypts only the current owner's last acknowledged continuation.
func (s *postgresStore) ReadExecutionSnapshot(ctx context.Context, account, app, id uuid.UUID, key []byte) (ExecutionSnapshot, error) {
	var state ExecutionSnapshot
	var wrapped, encrypted *string
	err := s.db.QueryRow(ctx, `SELECT revision,encrypted_dek,encrypted_snapshot FROM fused_unified_app_recovery WHERE execution_id=$1 AND account_id=$2 AND app_id=$3 AND current AND NOT ready AND lease_until>clock_timestamp() AND `+attemptAuthority, id, account, app).Scan(&state.Revision, &wrapped, &encrypted)
	// An expired attempt cannot inspect or mutate another owner's continuation.
	if err != nil {
		return state, err
	}
	state.State = json.RawMessage("null")
	// Absence represents a fresh run; an explicit saved null is distinguished by its revision.
	if encrypted != nil {
		state.State, err = openReplayEvidence(*wrapped, *encrypted, key)
	}
	return state, err
}

// SaveExecutionSnapshot acknowledges only a durable CAS under the current attempt fence.
func (s *postgresStore) SaveExecutionSnapshot(ctx context.Context, account, app, id uuid.UUID, revision int64, raw json.RawMessage, key []byte) (int64, error) {
	value, err := canonicalExecutionJSON(raw, true)
	// Snapshots share the existing bounded JSON contract but have independent durable storage.
	if err != nil {
		return 0, err
	}
	wrapped, encrypted, err := sealReplayEvidence(value, key)
	// Encryption must succeed before any continuation can be acknowledged.
	if err != nil {
		return 0, err
	}
	var next int64
	err = s.db.QueryRow(ctx, `UPDATE fused_unified_app_recovery SET encrypted_dek=$5,encrypted_snapshot=$6,revision=revision+1
 WHERE execution_id=$1 AND account_id=$2 AND app_id=$3 AND revision=$4 AND current AND NOT ready AND lease_until>clock_timestamp() AND `+attemptAuthority+` RETURNING revision`, id, account, app, revision, wrapped, encrypted).Scan(&next)
	// A CAS failure is not a successful save, even when another attempt advanced the run.
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrExecutionRecoveryConflict
	}
	return next, err
}

// DueExecutionRecoveries discovers bounded work; reservation rechecks every condition under a row lock.
func (s *postgresStore) DueExecutionRecoveries(ctx context.Context) ([]ExecutionResult, error) {
	rows, err := s.db.Query(ctx, `SELECT `+executionResultColumns+` FROM fused_unified_app_results WHERE id IN (
 SELECT c.execution_id FROM fused_unified_app_recovery c JOIN fused_unified_app_results r ON r.id=c.execution_id
 WHERE c.current AND (r.expires_at IS NULL OR r.expires_at>NOW())
 AND EXISTS(SELECT 1 FROM fused_app_families f JOIN fused_apps a ON a.app_id=r.app_id
 WHERE f.app_family_id=r.app_family_id AND f.account_id=r.account_id AND f.archived_at IS NULL
 AND f.unified_active_app_id=r.app_id AND a.status IN ('active','deprecated'))
 AND (r.app_token_id='00000000-0000-0000-0000-000000000000' OR EXISTS(SELECT 1 FROM fused_app_tokens t
 WHERE t.id=r.app_token_id AND t.app_family_id=r.app_family_id AND (t.expires_at IS NULL OR t.expires_at>NOW())))
 AND ((c.ready AND r.status='queued') OR (c.mode='automatic' AND c.attempt<c.max_attempts AND ((r.status IN ('failed','indeterminate') AND r.completed_at+make_interval(secs=>c.backoff_seconds)<=NOW())
 OR (r.status IN ('queued','running') AND c.lease_until<=NOW())))) ORDER BY c.lease_until LIMIT 32)`)
	// An unavailable queue must not be represented as a successful empty scan.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []ExecutionResult
	// SQL bounds both the retained payload count and the recovery batch size.
	for rows.Next() {
		record, err := scanExecutionResult(rows)
		// Partial records cannot authorize a background attempt.
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

// ListExecutionAttempts scopes a bounded history to the run selected by an authorized retained result.
func (s *postgresStore) ListExecutionAttempts(ctx context.Context, account, app, source uuid.UUID) ([]ExecutionAttemptSummary, error) {
	rows, err := s.db.Query(ctx, `SELECT c.execution_id,c.run_id,c.attempt,c.current,r.status,c.revision FROM fused_unified_app_recovery c JOIN fused_unified_app_results r ON r.id=c.execution_id
 WHERE c.account_id=$1 AND c.app_id=$2 AND c.run_id=(SELECT run_id FROM fused_unified_app_recovery WHERE execution_id=$3 AND account_id=$1 AND app_id=$2)
 AND (r.expires_at IS NULL OR r.expires_at>NOW()) ORDER BY c.attempt LIMIT 10`, account, app, source)
	// Storage failures cannot masquerade as empty execution history.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := make([]ExecutionAttemptSummary, 0)
	// The immutable attempt budget and SQL limit bound both decoding and the public response.
	for rows.Next() {
		var item ExecutionAttemptSummary
		// Incomplete rows must not invent a successful or current attempt.
		if err := rows.Scan(&item.ExecutionID, &item.RunID, &item.Attempt, &item.Current, &item.Status, &item.SnapshotRevision); err != nil {
			return nil, err
		}
		attempts = append(attempts, item)
	}
	return attempts, rows.Err()
}

// AuthorizeExecutionRecovery reloads current token policy without retaining or reconstructing bearer credentials.
func (s *postgresStore) AuthorizeExecutionRecovery(ctx context.Context, account, app, id uuid.UUID) (*AuthProjection, error) {
	var projection AuthProjection
	err := s.db.QueryRow(ctx, `SELECT r.account_id,r.app_family_id,r.app_id,r.app_token_id,r.app_version,f.kind,a.hosted_mcp,a.status,
 COALESCE(t.allow_all,true),COALESCE(t.allowed_operations,'{}'::text[]),t.expires_at,COALESCE(t.binding_mode,'dynamic')
 FROM fused_unified_app_results r JOIN fused_app_families f ON f.app_family_id=r.app_family_id AND f.account_id=r.account_id
 JOIN fused_apps a ON a.app_id=r.app_id LEFT JOIN fused_app_tokens t ON t.id=r.app_token_id AND t.app_family_id=r.app_family_id
 WHERE r.id=$1 AND r.account_id=$2 AND r.app_id=$3 AND f.archived_at IS NULL AND f.unified_active_app_id=r.app_id
 AND a.status IN ('active','deprecated') AND (r.expires_at IS NULL OR r.expires_at>NOW())
 AND ((r.app_token_id='00000000-0000-0000-0000-000000000000' AND (r.source_schedule_id IS NOT NULL OR r.source_webhook_event_id IS NOT NULL))
 OR (t.id IS NOT NULL AND (t.expires_at IS NULL OR t.expires_at>NOW())))`, id, account, app).Scan(&projection.AccountID, &projection.AppFamilyID, &projection.AppID, &projection.TokenID, &projection.Version, &projection.Kind, &projection.HostedMCP, &projection.AppStatus, &projection.TokenPolicy.AllowAll, &projection.TokenPolicy.AllowedOperations, &projection.TokenPolicy.ExpiresAt, &projection.BindingMode)
	// A revoked token or ambiguous delegated origin cannot authorize autonomous provider calls.
	if err != nil {
		return nil, err
	}
	return &projection, nil
}

// ClaimQueuedExecutionAttempt gives one scheduler ownership of a management-requested manual attempt.
func (s *postgresStore) ClaimQueuedExecutionAttempt(ctx context.Context, account, app, id uuid.UUID) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE fused_unified_app_recovery SET ready=false,lease_until=clock_timestamp()+INTERVAL '2 minutes'
 WHERE execution_id=$1 AND account_id=$2 AND app_id=$3 AND current AND ready AND `+attemptAuthority, id, account, app)
	// An unknown claim outcome cannot authorize a confined worker to start.
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// validateRecoveryOrigin rejects autonomous delegated work whose original consumer version was not retained.
func (s *postgresStore) validateRecoveryOrigin(ctx context.Context, record ExecutionResult) error {
	// Manual recovery can be authorized later by a fresh direct app token.
	if record.Recovery.Mode != "automatic" {
		return nil
	}
	// Engine-owned automation carries explicit durable provenance instead of a runtime token.
	if record.AppTokenID == uuid.Nil && (record.SourceScheduleID != nil || record.SourceWebhookEventID != "") {
		return nil
	}
	var direct bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_app_tokens WHERE id=$1 AND app_family_id=$2 AND (expires_at IS NULL OR expires_at>NOW()))`, record.AppTokenID, record.AppFamilyID).Scan(&direct)
	// Storage uncertainty cannot authorize a second execution after a crash.
	if err != nil {
		return err
	}
	if !direct {
		return ErrExecutionRecoveryOrigin
	} // A consumer token alone cannot reconstruct its original delegation.
	return nil
}
