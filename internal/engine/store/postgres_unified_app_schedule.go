package store

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const scheduleColumns = `id,account_id,app_family_id,name,cron,timezone,run_at,enabled,revision,next_run_at,
	last_execution_id,last_app_id,last_status,last_started_at,created_by,created_at,encrypted_dek,encrypted_input`

// scanSchedule decrypts only explicitly authorized management or internal worker reads.
func scanSchedule(row pgx.Row, key []byte) (UnifiedAppSchedule, error) {
	var item UnifiedAppSchedule
	var wrapped, encrypted string
	err := row.Scan(&item.ID, &item.AccountID, &item.FamilyID, &item.Name, &item.Cron, &item.Timezone, &item.RunAt,
		&item.Enabled, &item.Revision, &item.NextRunAt, &item.LastExecutionID, &item.LastAppID, &item.LastStatus,
		&item.LastStartedAt, &item.CreatedBy, &item.CreatedAt, &wrapped, &encrypted)
	// Partial records cannot establish execution authority.
	if err != nil {
		return item, err
	}
	item.Input, err = openReplayEvidence(wrapped, encrypted, key)
	return item, err
}

// ListUnifiedAppSchedules exposes at most the enforced per-family limit under exact account ownership.
func (s *postgresStore) ListUnifiedAppSchedules(ctx context.Context, account, family uuid.UUID, key []byte) ([]UnifiedAppSchedule, error) {
	rows, err := s.db.Query(ctx, `SELECT `+scheduleColumns+` FROM fused_unified_app_schedules WHERE account_id=$1 AND app_family_id=$2 ORDER BY created_at,id LIMIT 100`, account, family)
	// Storage errors must not appear as an empty schedule list.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]UnifiedAppSchedule, 0)
	// Each result uses the same private decryption boundary as the worker.
	for rows.Next() {
		item, err := scanSchedule(rows, key)
		// An incomplete listing cannot safely drive edits or deletes.
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SaveUnifiedAppSchedule encrypts inputs and serializes family capacity and optimistic edits in one transaction.
func (s *postgresStore) SaveUnifiedAppSchedule(ctx context.Context, item UnifiedAppSchedule, key []byte) error {
	// Storage callers cannot bypass the same timing and payload validation as HTTP callers.
	if err := ValidateSchedule(&item, time.Now()); err != nil {
		return err
	}
	wrapped, encrypted, err := sealReplayEvidence(item.Input, key)
	// Encryption must succeed before any durable configuration is changed.
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	// The family lock serializes the schedule cap across concurrent managers.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var family uuid.UUID
	err = tx.QueryRow(ctx, `SELECT app_family_id FROM fused_app_families WHERE account_id=$1 AND app_family_id=$2 AND kind='unified_app' AND archived_at IS NULL FOR UPDATE`, item.AccountID, item.FamilyID).Scan(&family)
	// Archived and foreign families must never retain fresh automatic authority.
	if err != nil {
		return err
	}
	// A configuration change commits only after the revision and family capacity checks pass.
	if err := writeScheduleConfiguration(ctx, tx, item, wrapped, encrypted); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// writeScheduleConfiguration applies creation limits and optimistic edits under the locked family.
func writeScheduleConfiguration(ctx context.Context, tx pgx.Tx, item UnifiedAppSchedule, wrapped, encrypted string) error {
	// Revision zero is creation; client-generated IDs fence uncertain retries without duplicating schedules.
	if item.Revision == 0 {
		var count int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM fused_unified_app_schedules WHERE app_family_id=$1`, item.FamilyID).Scan(&count)
		// The finite family cap bounds worker and UI work even for hostile clients.
		if err != nil {
			return err
		}
		if count >= 100 {
			return errors.New("a Unified App can have at most 100 schedules")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO fused_unified_app_schedules (id,account_id,app_family_id,name,cron,timezone,run_at,enabled,next_run_at,encrypted_dek,encrypted_input,created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (id) DO NOTHING`, item.ID, item.AccountID, item.FamilyID, item.Name, item.Cron, item.Timezone, item.RunAt, item.Enabled, item.NextRunAt, wrapped, encrypted, item.CreatedBy)
		// A repeated ID is reviewable instead of producing another scheduled effect.
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrScheduleConflict
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE fused_unified_app_schedules SET name=$4,cron=$5,timezone=$6,run_at=$7,enabled=$8,next_run_at=$9,encrypted_dek=$10,encrypted_input=$11,revision=revision+1
			WHERE id=$1 AND account_id=$2 AND app_family_id=$3 AND revision=$12 AND last_status<>'running'`, item.ID, item.AccountID, item.FamilyID, item.Name, item.Cron, item.Timezone, item.RunAt, item.Enabled, item.NextRunAt, wrapped, encrypted, item.Revision)
		// A concurrent dispatch or edit requires a fresh reviewed snapshot.
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrScheduleConflict
		}
	}
	return nil
}

// DeleteUnifiedAppSchedule removes future automation without changing retained execution history.
func (s *postgresStore) DeleteUnifiedAppSchedule(ctx context.Context, account, family, id uuid.UUID, revision int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_unified_app_schedules WHERE id=$1 AND account_id=$2 AND app_family_id=$3 AND revision=$4 AND last_status<>'running'`, id, account, family, revision)
	// A stale or in-flight schedule must be refreshed before it can be removed.
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrScheduleConflict
	}
	return nil
}

// DueUnifiedAppSchedules performs bounded discovery; only the later atomic reservation owns an occurrence.
func (s *postgresStore) DueUnifiedAppSchedules(ctx context.Context, key []byte) ([]UnifiedAppSchedule, error) {
	rows, err := s.db.Query(ctx, `SELECT `+scheduleColumns+` FROM fused_unified_app_schedules schedule
		WHERE enabled AND next_run_at<=NOW() AND last_status<>'running'
		AND EXISTS (SELECT 1 FROM fused_app_families family JOIN fused_apps app ON app.app_id=family.unified_active_app_id
			WHERE family.app_family_id=schedule.app_family_id AND family.account_id=schedule.account_id
			AND family.archived_at IS NULL AND app.status IN ('active','deprecated'))
		ORDER BY next_run_at,id LIMIT 32`)
	// A failed discovery cannot start work from a stale snapshot.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []UnifiedAppSchedule
	// Decryption failures stay visible and never turn corrupted input into null.
	for rows.Next() {
		item, err := scanSchedule(rows, key)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReserveScheduledExecution advances the schedule and records its run before any external side effect.
func (s *postgresStore) ReserveScheduledExecution(ctx context.Context, item UnifiedAppSchedule, record ExecutionResult) (bool, error) {
	// The occurrence must carry the same schedule, tenant, and family as its accepted result.
	if !validScheduledRecord(item, record) {
		return false, ErrExecutionResultInvalid
	}

	tx, err := s.db.Begin(ctx)
	// A transaction is necessary to avoid losing an occurrence between advancement and result creation.
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var active uuid.UUID
	err = tx.QueryRow(ctx, `SELECT unified_active_app_id FROM fused_app_families WHERE account_id=$1 AND app_family_id=$2 AND kind='unified_app' AND archived_at IS NULL FOR SHARE`, item.AccountID, item.FamilyID).Scan(&active)
	// Promotion and deletion cannot admit a stale version under current traffic authority.
	if err != nil {
		return false, err
	}
	if active != record.AppID {
		return false, ErrAppDeactivated
	}
	var next *time.Time
	// Coalesce missed recurring times into one run, then advance to a future wall-clock occurrence.
	if item.Cron != "" {
		value, err := NextScheduleTime(item.Cron, item.Timezone, time.Now())
		if err != nil {
			return false, err
		}
		next = &value
	}
	tag, err := tx.Exec(ctx, `UPDATE fused_unified_app_schedules SET next_run_at=$5,last_execution_id=$6,last_app_id=$7,last_status='running',last_started_at=NOW(),revision=revision+1
		WHERE id=$1 AND account_id=$2 AND app_family_id=$3 AND revision=$4 AND enabled AND last_status<>'running' AND next_run_at=$8 AND next_run_at<=NOW()`, item.ID, item.AccountID, item.FamilyID, item.Revision, next, record.ID, record.AppID, item.NextRunAt)
	// Exactly one replica wins the compare-and-swap; every other observer leaves without running code.
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	writer := &postgresStore{db: workspaceTransaction{Tx: tx}}
	// A result failure rolls back schedule advancement, allowing a later safe admission attempt.
	if err := writer.CreateExecutionResult(ctx, record); err != nil {
		return false, err
	}
	err = tx.Commit(ctx)
	return err == nil, err
}

// validScheduledRecord prevents an internal caller from reserving another schedule's execution authority.
func validScheduledRecord(item UnifiedAppSchedule, record ExecutionResult) bool {
	return record.SourceScheduleID != nil && *record.SourceScheduleID == item.ID && record.AccountID == item.AccountID && record.AppFamilyID == item.FamilyID && item.NextRunAt != nil && bytes.Equal(item.Input, record.Input)
}

// FinishScheduledExecution updates only the occurrence that still owns the schedule's running marker.
func (s *postgresStore) FinishScheduledExecution(ctx context.Context, id, executionID uuid.UUID, status string) error {
	// Fixed terminal values prevent raw provider messages from entering the schedule summary.
	if status != "succeeded" && status != "failed" && status != "indeterminate" {
		return ErrExecutionResultInvalid
	}
	_, err := s.db.Exec(ctx, `UPDATE fused_unified_app_schedules SET last_status=$3,revision=revision+1,
		enabled=CASE WHEN $3='indeterminate' THEN false ELSE enabled END
		WHERE id=$1 AND last_execution_id=$2 AND last_status='running'`, id, executionID, status)
	return err
}

// RecoverScheduledExecutions pauses abandoned work; automatic retries could repeat irreversible provider effects.
func (s *postgresStore) RecoverScheduledExecutions(ctx context.Context) ([]ExecutionResult, error) {
	rows, err := s.db.Query(ctx, `WITH abandoned AS (
		UPDATE fused_unified_app_schedules SET last_status='indeterminate',enabled=false,revision=revision+1
		WHERE last_status='running' AND last_started_at<NOW()-INTERVAL '5 minutes' RETURNING last_execution_id
	) UPDATE fused_unified_app_results SET status='indeterminate',error_code='scheduled_run_interrupted',
		error_message='Scheduled run was interrupted; review provider state before retrying',completed_at=NOW(),updated_at=NOW(),expires_at=NOW()+INTERVAL '24 hours'
		WHERE id IN (SELECT last_execution_id FROM abandoned) AND status IN ('queued','running') RETURNING `+executionResultColumns)
	// Recovered terminal records feed the same canonical publisher as normal completed runs.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []ExecutionResult
	for rows.Next() {
		record, err := scanExecutionResult(rows)
		// Partial records cannot become fabricated execution receipts.
		if err != nil {
			return nil, err
		}
		results = append(results, *record)
	}
	return results, rows.Err()
}
