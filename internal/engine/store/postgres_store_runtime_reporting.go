package store

import (
	"context"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *postgresStore) SaveRuntimeEntitlement(ctx context.Context, entitlement models.RuntimeEntitlement) error {
	entitlement = entitlement.Normalized()
	query := `
		INSERT INTO fused_runtime_entitlements (
			singleton_key, plan, heartbeat_required, usage_reporting, public_service_insights_reporting,
			heartbeat_interval_seconds, heartbeat_stale_after_seconds, refreshed_at, updated_at
		)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (singleton_key) DO UPDATE SET
			plan = EXCLUDED.plan,
			heartbeat_required = EXCLUDED.heartbeat_required,
			usage_reporting = EXCLUDED.usage_reporting,
			public_service_insights_reporting = EXCLUDED.public_service_insights_reporting,
			heartbeat_interval_seconds = EXCLUDED.heartbeat_interval_seconds,
			heartbeat_stale_after_seconds = EXCLUDED.heartbeat_stale_after_seconds,
			refreshed_at = EXCLUDED.refreshed_at,
			updated_at = NOW()
	`
	_, err := s.db.Exec(ctx, query,
		entitlement.Plan,
		entitlement.HeartbeatRequired,
		entitlement.UsageReporting,
		entitlement.PublicServiceInsightsReporting,
		entitlement.HeartbeatIntervalSeconds,
		entitlement.HeartbeatStaleAfterSeconds,
		entitlement.RefreshedAt,
	)
	return err
}

func (s *postgresStore) GetRuntimeEntitlement(ctx context.Context) (models.RuntimeEntitlement, error) {
	query := `
		SELECT plan, heartbeat_required, usage_reporting, public_service_insights_reporting,
			heartbeat_interval_seconds, heartbeat_stale_after_seconds, refreshed_at
		FROM fused_runtime_entitlements
		WHERE singleton_key = 1
	`
	var entitlement models.RuntimeEntitlement
	err := s.db.QueryRow(ctx, query).Scan(
		&entitlement.Plan,
		&entitlement.HeartbeatRequired,
		&entitlement.UsageReporting,
		&entitlement.PublicServiceInsightsReporting,
		&entitlement.HeartbeatIntervalSeconds,
		&entitlement.HeartbeatStaleAfterSeconds,
		&entitlement.RefreshedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return models.DefaultRuntimeEntitlement(), nil
		}
		return models.RuntimeEntitlement{}, err
	}
	return entitlement.Normalized(), nil
}

func (s *postgresStore) IncrementRuntimeUsageCounters(ctx context.Context, increments []models.EngineUsageIncrement) error {
	if len(increments) == 0 {
		return nil
	}
	query := `
		INSERT INTO fused_engine_usage_counter_reports (
			metric, bucket_start, bucket_seconds, count, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (metric, bucket_start, bucket_seconds) WHERE flushed_at IS NULL
		DO UPDATE SET
			count = fused_engine_usage_counter_reports.count + EXCLUDED.count,
			updated_at = NOW()
	`
	batch := &pgx.Batch{}
	for _, increment := range increments {
		batch.Queue(query, increment.Metric, increment.BucketStart, increment.BucketSeconds, increment.Count)
	}
	return s.db.SendBatch(ctx, batch).Close()
}

func (s *postgresStore) ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error) {
	if limit <= 0 {
		limit = 500
	}
	query := `
		SELECT report_id, metric, bucket_start, bucket_seconds, count
		FROM fused_engine_usage_counter_reports
		WHERE flushed_at IS NULL
		ORDER BY bucket_start ASC, created_at ASC
		LIMIT $1
	`
	rows, err := s.db.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reports := []models.EngineUsageReport{}
	for rows.Next() {
		var report models.EngineUsageReport
		if err := rows.Scan(&report.ReportID, &report.Metric, &report.BucketStart, &report.BucketSeconds, &report.Count); err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

func (s *postgresStore) MarkRuntimeUsageReportsFlushed(ctx context.Context, reportIDs []uuid.UUID, flushedAt time.Time) error {
	if len(reportIDs) == 0 {
		return nil
	}
	// The WHERE keeps this idempotent if a shutdown retry races a previous
	// successful mark; already-flushed rows are intentionally left untouched.
	_, err := s.db.Exec(ctx, `
		UPDATE fused_engine_usage_counter_reports
		SET flushed_at = $2, updated_at = NOW()
		WHERE report_id = ANY($1::uuid[]) AND flushed_at IS NULL
	`, reportIDs, flushedAt)
	return err
}
