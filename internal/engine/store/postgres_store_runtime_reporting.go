package store

import (
	"context"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SaveRuntimeEntitlement atomically persists the complete normalized Registry contract used by local admission.
func (s *postgresStore) SaveRuntimeEntitlement(ctx context.Context, entitlement models.RuntimeEntitlement) error {
	entitlement = entitlement.Normalized()
	query := `
		INSERT INTO fused_runtime_entitlements (
			singleton_key, entitlement_revision, plan, heartbeat_required, usage_reporting,
			public_service_insights_enabled,
			heartbeat_interval_seconds, heartbeat_stale_after_seconds, refreshed_at, updated_at,
			max_buckets, max_api_families, max_sdk_families, max_mcp_families, max_services, max_sandbox_concurrency,
			drift_monitoring_enabled, webhook_ingestion_enabled, sso_enabled, execution_retention_days
		)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, NOW(), $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (singleton_key) DO UPDATE SET
			entitlement_revision = EXCLUDED.entitlement_revision,
			plan = EXCLUDED.plan,
			heartbeat_required = EXCLUDED.heartbeat_required,
			usage_reporting = EXCLUDED.usage_reporting,
			public_service_insights_enabled = EXCLUDED.public_service_insights_enabled,
			heartbeat_interval_seconds = EXCLUDED.heartbeat_interval_seconds,
			heartbeat_stale_after_seconds = EXCLUDED.heartbeat_stale_after_seconds,
			refreshed_at = EXCLUDED.refreshed_at,
			updated_at = NOW(),
			max_buckets = EXCLUDED.max_buckets,
			max_api_families = EXCLUDED.max_api_families,
			max_sdk_families = EXCLUDED.max_sdk_families,
			max_mcp_families = EXCLUDED.max_mcp_families,
			max_services = EXCLUDED.max_services,
			max_sandbox_concurrency = EXCLUDED.max_sandbox_concurrency,
			drift_monitoring_enabled = EXCLUDED.drift_monitoring_enabled,
			webhook_ingestion_enabled = EXCLUDED.webhook_ingestion_enabled,
			sso_enabled = EXCLUDED.sso_enabled,
			execution_retention_days = EXCLUDED.execution_retention_days
	`
	_, err := s.db.Exec(ctx, query,
		entitlement.EntitlementRevision,
		entitlement.Plan,
		entitlement.HeartbeatRequired,
		entitlement.UsageReporting,
		entitlement.PublicServiceInsightsEnabled,
		entitlement.HeartbeatIntervalSeconds,
		entitlement.HeartbeatStaleAfterSeconds,
		entitlement.RefreshedAt,
		entitlement.MaxBuckets,
		entitlement.MaxAPIFamilies,
		entitlement.MaxSDKFamilies,
		entitlement.MaxMCPFamilies,
		entitlement.MaxServices,
		entitlement.MaxSandboxConcurrency,
		entitlement.DriftMonitoringEnabled,
		entitlement.WebhookIngestionEnabled,
		entitlement.SSOEnabled,
		entitlement.ExecutionRetentionDays,
	)
	return err
}

// GetRuntimeEntitlement loads the complete local contract and applies compatibility defaults for absent state.
func (s *postgresStore) GetRuntimeEntitlement(ctx context.Context) (models.RuntimeEntitlement, error) {
	query := `
		SELECT entitlement_revision, plan, heartbeat_required, usage_reporting,
			public_service_insights_enabled,
			heartbeat_interval_seconds, heartbeat_stale_after_seconds, refreshed_at,
			max_buckets, max_api_families, max_sdk_families, max_mcp_families, max_services, max_sandbox_concurrency,
			drift_monitoring_enabled, webhook_ingestion_enabled, sso_enabled, execution_retention_days
		FROM fused_runtime_entitlements
		WHERE singleton_key = 1
	`
	var entitlement models.RuntimeEntitlement
	err := s.db.QueryRow(ctx, query).Scan(
		&entitlement.EntitlementRevision,
		&entitlement.Plan,
		&entitlement.HeartbeatRequired,
		&entitlement.UsageReporting,
		&entitlement.PublicServiceInsightsEnabled,
		&entitlement.HeartbeatIntervalSeconds,
		&entitlement.HeartbeatStaleAfterSeconds,
		&entitlement.RefreshedAt,
		&entitlement.MaxBuckets,
		&entitlement.MaxAPIFamilies,
		&entitlement.MaxSDKFamilies,
		&entitlement.MaxMCPFamilies,
		&entitlement.MaxServices,
		&entitlement.MaxSandboxConcurrency,
		&entitlement.DriftMonitoringEnabled,
		&entitlement.WebhookIngestionEnabled,
		&entitlement.SSOEnabled,
		&entitlement.ExecutionRetentionDays,
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
