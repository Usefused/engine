package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

// ListUnprojectedPublicInsightServiceIDs excludes logical envelopes from provider reliability reports.
func (s *postgresStore) ListUnprojectedPublicInsightServiceIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT event.service_id
		FROM fused_engine_execution_events event
		LEFT JOIN fused_public_service_insight_projected_events projected ON projected.event_id = event.id
		WHERE projected.event_id IS NULL AND event.started_at < $1
		  AND event.direction = 'outbound' AND event.operation_id IS NOT NULL AND event.execution_kind = 'physical'
		ORDER BY event.service_id LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list public insight candidate services: %w", err)
	}
	defer rows.Close()
	serviceIDs := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var serviceID uuid.UUID
		if err := rows.Scan(&serviceID); err != nil {
			return nil, fmt.Errorf("scan public insight candidate service: %w", err)
		}
		serviceIDs = append(serviceIDs, serviceID)
	}
	return serviceIDs, rows.Err()
}

func (s *postgresStore) ProjectPublicServiceInsightReports(ctx context.Context, reportableServiceIDs []uuid.UUID, before time.Time, eventLimit int) (int64, error) {
	if len(reportableServiceIDs) == 0 {
		return 0, nil
	}
	result, err := s.db.Exec(ctx, publicInsightProjectionSQL, reportableServiceIDs, before, eventLimit)
	if err != nil {
		return 0, fmt.Errorf("project public service insights: %w", err)
	}
	return result.RowsAffected(), nil
}

const publicInsightProjectionSQL = `
	WITH selected AS MATERIALIZED (
		SELECT event.*, date_trunc('hour', event.started_at) AS bucket_start,
		       COALESCE(NULLIF(event.provider_status_class, ''), 'none') AS bounded_status_class
		FROM fused_engine_execution_events event
		LEFT JOIN fused_public_service_insight_projected_events projected ON projected.event_id = event.id
		WHERE projected.event_id IS NULL AND event.service_id = ANY($1::uuid[])
		  AND event.started_at < $2 AND event.direction = 'outbound' AND event.operation_id IS NOT NULL
		  AND event.execution_kind = 'physical'
		ORDER BY event.started_at, event.id LIMIT $3
		FOR UPDATE OF event SKIP LOCKED
	), grouped AS (
		SELECT service_id, service_version_id::uuid, operation_id, direction, transport, status,
		       bounded_status_class, bucket_start, COUNT(*) AS call_count,
		       SUM(latency_ms) AS total_latency_ms_sum,
		       SUM(COALESCE(provider_latency_ms, 0)) AS provider_latency_ms_sum,
		       SUM(GREATEST(attempt_count, 1)) AS retry_attempts_sum,
		       jsonb_build_array(
			COUNT(*) FILTER (WHERE latency_ms <= 5), COUNT(*) FILTER (WHERE latency_ms > 5 AND latency_ms <= 10),
			COUNT(*) FILTER (WHERE latency_ms > 10 AND latency_ms <= 25), COUNT(*) FILTER (WHERE latency_ms > 25 AND latency_ms <= 50),
			COUNT(*) FILTER (WHERE latency_ms > 50 AND latency_ms <= 100), COUNT(*) FILTER (WHERE latency_ms > 100 AND latency_ms <= 250),
			COUNT(*) FILTER (WHERE latency_ms > 250 AND latency_ms <= 500), COUNT(*) FILTER (WHERE latency_ms > 500 AND latency_ms <= 1000),
			COUNT(*) FILTER (WHERE latency_ms > 1000 AND latency_ms <= 2500), COUNT(*) FILTER (WHERE latency_ms > 2500 AND latency_ms <= 5000),
			COUNT(*) FILTER (WHERE latency_ms > 5000 AND latency_ms <= 10000), COUNT(*) FILTER (WHERE latency_ms > 10000)
		) AS latency_histogram
		FROM selected
		GROUP BY service_id, service_version_id, operation_id, direction, transport, status, bounded_status_class, bucket_start
	), inserted AS (
		INSERT INTO fused_public_service_insight_outbox (
			service_id, service_version_id, registry_object_kind, registry_object_id,
			direction, transport, outcome, provider_status_class, bucket_start,
			call_count, total_latency_ms_sum, provider_latency_ms_sum, latency_histogram, retry_attempts_sum
		)
		SELECT service_id, service_version_id, 'endpoint', operation_id, direction, transport,
		       status, bounded_status_class, bucket_start, call_count, total_latency_ms_sum,
		       provider_latency_ms_sum, latency_histogram, retry_attempts_sum
		FROM grouped
		RETURNING report_id, service_id, service_version_id, registry_object_id,
		          direction, transport, outcome, provider_status_class, bucket_start
	)
	INSERT INTO fused_public_service_insight_projected_events (event_id, report_id)
	SELECT selected.id, inserted.report_id
	FROM selected
	JOIN inserted ON inserted.service_id = selected.service_id
	 AND inserted.service_version_id = selected.service_version_id::uuid
	 AND inserted.registry_object_id = selected.operation_id
	 AND inserted.direction = selected.direction AND inserted.transport = selected.transport
	 AND inserted.outcome = selected.status
	 AND inserted.provider_status_class = selected.bounded_status_class
	 AND inserted.bucket_start = selected.bucket_start`

func (s *postgresStore) ListPendingPublicServiceInsightReports(ctx context.Context, limit int, now time.Time) ([]models.PublicServiceInsightReport, error) {
	rows, err := s.db.Query(ctx, `
		SELECT report_id, service_id, service_version_id, registry_object_kind, registry_object_id,
		       direction, transport, outcome, provider_status_class, bucket_start, bucket_seconds,
		       call_count, total_latency_ms_sum, provider_latency_ms_sum, latency_histogram, retry_attempts_sum
		FROM fused_public_service_insight_outbox
		WHERE state = 'pending' AND next_attempt_at <= $1
		ORDER BY created_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending public insight reports: %w", err)
	}
	defer rows.Close()
	reports := make([]models.PublicServiceInsightReport, 0, limit)
	for rows.Next() {
		var report models.PublicServiceInsightReport
		var histogram []byte
		if err := rows.Scan(
			&report.ReportID, &report.ServiceID, &report.ServiceVersionID, &report.RegistryObjectKind, &report.RegistryObjectID,
			&report.Direction, &report.Transport, &report.Outcome, &report.ProviderStatusClass, &report.BucketStart,
			&report.BucketSeconds, &report.CallCount, &report.TotalLatencyMsSum, &report.ProviderLatencyMsSum,
			&histogram, &report.RetryAttemptsSum,
		); err != nil {
			return nil, fmt.Errorf("scan pending public insight report: %w", err)
		}
		if err := json.Unmarshal(histogram, &report.LatencyHistogram); err != nil {
			return nil, fmt.Errorf("decode public insight histogram: %w", err)
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

func (s *postgresStore) MarkPublicServiceInsightReportResults(ctx context.Context, results []models.PublicServiceInsightReportResult, at time.Time) error {
	if len(results) == 0 {
		return nil
	}
	payload, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("encode public insight results: %w", err)
	}
	_, err = s.db.Exec(ctx, `
		UPDATE fused_public_service_insight_outbox report
		SET state = CASE WHEN result.accepted THEN 'sent' ELSE 'rejected' END,
		    sent_at = CASE WHEN result.accepted THEN $2::timestamptz ELSE NULL::timestamptz END,
		    last_error_code = COALESCE(result.reason, ''), attempt_count = attempt_count + 1,
		    updated_at = $2::timestamptz
		FROM jsonb_to_recordset($1::jsonb) AS result(report_id uuid, accepted boolean, reason text)
		WHERE report.report_id = result.report_id AND report.state = 'pending'`, payload, at)
	if err != nil {
		return fmt.Errorf("mark public insight report results: %w", err)
	}
	return nil
}

func (s *postgresStore) MarkPublicServiceInsightReportDeliveryFailure(ctx context.Context, reportIDs []uuid.UUID, errorCode string, at time.Time) error {
	if len(reportIDs) == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		UPDATE fused_public_service_insight_outbox
		SET attempt_count = attempt_count + 1, last_error_code = $2,
		    next_attempt_at = $3 + LEAST(power(2, LEAST(attempt_count, 6)), 60) * INTERVAL '1 minute', updated_at = $3
		WHERE report_id = ANY($1::uuid[]) AND state = 'pending'`, reportIDs, errorCode, at)
	if err != nil {
		return fmt.Errorf("mark public insight delivery failure: %w", err)
	}
	return nil
}
