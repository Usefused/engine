package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

const workspaceExecutionBreakdownLimit = 25

// GetWorkspaceExecutionAnalytics reads the workspace overview in two bounded
// queries regardless of receipt, service, app, or bucket cardinality.
func (s *postgresStore) GetWorkspaceExecutionAnalytics(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time) (models.WorkspaceExecutionAnalytics, error) {
	var analytics models.WorkspaceExecutionAnalytics
	if err := s.scanWorkspaceExecutionSummary(ctx, accountID, startDate, endDate, &analytics); err != nil {
		return models.WorkspaceExecutionAnalytics{}, err
	}
	byService, err := s.listWorkspaceServiceExecutionBreakdown(ctx, accountID, startDate, endDate)
	if err != nil {
		return models.WorkspaceExecutionAnalytics{}, err
	}
	analytics.ByService = byService
	return analytics, nil
}

// scanWorkspaceExecutionSummary selects totals and each ranked highlight in
// SQL so the application never filters or sorts receipt-derived candidates.
func (s *postgresStore) scanWorkspaceExecutionSummary(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time, analytics *models.WorkspaceExecutionAnalytics) error {
	var mostUsedSDK, mostUsedService models.EngineExecutionBreakdown
	var mostFailedService, mostUsedBucket models.EngineExecutionBreakdown
	err := s.db.QueryRow(ctx, `
		WITH filtered AS (
			SELECT event.account_id, event.app_family_id, event.transport, event.direction,
			       event.service_id, event.status, event.latency_ms
			FROM fused_engine_execution_events event
			WHERE event.account_id = $1 AND event.started_at >= $2 AND event.started_at <= $3
		), summary AS (
			SELECT COUNT(*) AS total_calls,
			       COUNT(*) FILTER (WHERE direction = 'inbound') AS inbound_calls,
			       COUNT(*) FILTER (WHERE status = 'success') AS successful_calls,
			       COUNT(*) FILTER (WHERE status = 'failed') AS failed_calls,
			       COALESCE(AVG(latency_ms), 0) AS average_latency_ms,
			       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0) AS median_latency_ms,
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0) AS p95_latency_ms
			FROM filtered
		), service_usage AS (
			SELECT event.service_id::text AS key,
			       COALESCE(NULLIF(service.service_name, ''), NULLIF(service.service_slug, ''), 'Service metadata unavailable') AS label,
			       COUNT(*) AS total_calls,
			       COUNT(*) FILTER (WHERE event.direction = 'inbound') AS inbound_calls,
			       COUNT(*) FILTER (WHERE event.status = 'failed') AS failed_calls,
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY event.latency_ms), 0) AS p95_latency_ms
			FROM filtered event
			LEFT JOIN fused_workspace_services service ON service.service_id = event.service_id
			WHERE event.service_id IS NOT NULL
			GROUP BY event.service_id, service.service_name, service.service_slug
		), sdk_usage AS (
			SELECT event.app_family_id::text AS key,
			       COALESCE(NULLIF(family.display_name, ''), 'SDK metadata unavailable') AS label,
			       COUNT(*) AS total_calls,
			       COUNT(*) FILTER (WHERE event.direction = 'inbound') AS inbound_calls,
			       COUNT(*) FILTER (WHERE event.status = 'failed') AS failed_calls,
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY event.latency_ms), 0) AS p95_latency_ms
			FROM filtered event
			LEFT JOIN fused_app_families family
			  ON family.app_family_id = event.app_family_id AND family.account_id = event.account_id
			WHERE event.transport IN ('sdk', 'rest') AND event.app_family_id IS NOT NULL
			GROUP BY event.app_family_id, family.display_name
		), bucket_usage AS (
			SELECT family_bucket.bucket_id::text AS key,
			       COALESCE(NULLIF(bucket.name, ''), 'Bucket metadata unavailable') AS label,
			       COUNT(*) AS total_calls,
			       COUNT(*) FILTER (WHERE event.direction = 'inbound') AS inbound_calls,
			       COUNT(*) FILTER (WHERE event.status = 'failed') AS failed_calls,
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY event.latency_ms), 0) AS p95_latency_ms
			FROM filtered event
			JOIN fused_app_family_buckets family_bucket ON family_bucket.app_family_id = event.app_family_id
			JOIN fused_buckets bucket ON bucket.id = family_bucket.bucket_id
			WHERE event.transport IN ('sdk', 'mcp', 'rest')
			GROUP BY family_bucket.bucket_id, bucket.name
		), most_used_sdk AS (
			SELECT * FROM sdk_usage ORDER BY total_calls DESC, label, key LIMIT 1
		), most_used_service AS (
			SELECT * FROM service_usage ORDER BY total_calls DESC, label, key LIMIT 1
		), most_failed_service AS (
			SELECT * FROM service_usage WHERE failed_calls > 0 ORDER BY failed_calls DESC, total_calls DESC, label, key LIMIT 1
		), most_used_bucket AS (
			SELECT * FROM bucket_usage ORDER BY total_calls DESC, label, key LIMIT 1
		)
		SELECT summary.total_calls, summary.inbound_calls, summary.successful_calls, summary.failed_calls,
		       summary.average_latency_ms, summary.median_latency_ms, summary.p95_latency_ms,
		       COALESCE(sdk.key, ''), COALESCE(sdk.label, ''), COALESCE(sdk.total_calls, 0), COALESCE(sdk.inbound_calls, 0), COALESCE(sdk.failed_calls, 0), COALESCE(sdk.p95_latency_ms, 0),
		       COALESCE(service.key, ''), COALESCE(service.label, ''), COALESCE(service.total_calls, 0), COALESCE(service.inbound_calls, 0), COALESCE(service.failed_calls, 0), COALESCE(service.p95_latency_ms, 0),
		       COALESCE(failed.key, ''), COALESCE(failed.label, ''), COALESCE(failed.total_calls, 0), COALESCE(failed.inbound_calls, 0), COALESCE(failed.failed_calls, 0), COALESCE(failed.p95_latency_ms, 0),
		       COALESCE(bucket.key, ''), COALESCE(bucket.label, ''), COALESCE(bucket.total_calls, 0), COALESCE(bucket.inbound_calls, 0), COALESCE(bucket.failed_calls, 0), COALESCE(bucket.p95_latency_ms, 0)
		FROM summary
		LEFT JOIN most_used_sdk sdk ON true
		LEFT JOIN most_used_service service ON true
		LEFT JOIN most_failed_service failed ON true
		LEFT JOIN most_used_bucket bucket ON true`, accountID, startDate, endDate).Scan(
		&analytics.TotalCalls, &analytics.InboundCalls, &analytics.SuccessfulCalls, &analytics.FailedCalls,
		&analytics.AverageLatencyMs, &analytics.MedianLatencyMs, &analytics.P95LatencyMs,
		&mostUsedSDK.Key, &mostUsedSDK.Label, &mostUsedSDK.TotalCalls, &mostUsedSDK.InboundCalls, &mostUsedSDK.FailedCalls, &mostUsedSDK.P95LatencyMs,
		&mostUsedService.Key, &mostUsedService.Label, &mostUsedService.TotalCalls, &mostUsedService.InboundCalls, &mostUsedService.FailedCalls, &mostUsedService.P95LatencyMs,
		&mostFailedService.Key, &mostFailedService.Label, &mostFailedService.TotalCalls, &mostFailedService.InboundCalls, &mostFailedService.FailedCalls, &mostFailedService.P95LatencyMs,
		&mostUsedBucket.Key, &mostUsedBucket.Label, &mostUsedBucket.TotalCalls, &mostUsedBucket.InboundCalls, &mostUsedBucket.FailedCalls, &mostUsedBucket.P95LatencyMs,
	)
	if err != nil {
		return fmt.Errorf("get workspace execution summary: %w", err)
	}
	analytics.MostUsedSDK = nonEmptyExecutionBreakdown(mostUsedSDK)
	analytics.MostUsedService = nonEmptyExecutionBreakdown(mostUsedService)
	analytics.MostFailedService = nonEmptyExecutionBreakdown(mostFailedService)
	analytics.MostUsedBucket = nonEmptyExecutionBreakdown(mostUsedBucket)
	return nil
}

// nonEmptyExecutionBreakdown converts an absent SQL highlight into GraphQL
// null while preserving zero-valued metrics for a real grouped identity.
func nonEmptyExecutionBreakdown(item models.EngineExecutionBreakdown) *models.EngineExecutionBreakdown {
	if item.Key == "" {
		return nil
	}
	return &item
}

// listWorkspaceServiceExecutionBreakdown returns only the highest-traffic
// services and keeps account, range, grouping, ranking, and limit in SQL.
func (s *postgresStore) listWorkspaceServiceExecutionBreakdown(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time) ([]models.EngineExecutionBreakdown, error) {
	rows, err := s.db.Query(ctx, `
		SELECT event.service_id::text,
		       COALESCE(NULLIF(service.service_name, ''), NULLIF(service.service_slug, ''), 'Service metadata unavailable'),
		       COUNT(*), COUNT(*) FILTER (WHERE event.direction = 'inbound'),
		       COUNT(*) FILTER (WHERE event.status = 'failed'),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY event.latency_ms), 0)
		FROM fused_engine_execution_events event
		LEFT JOIN fused_workspace_services service ON service.service_id = event.service_id
		WHERE event.account_id = $1 AND event.started_at >= $2 AND event.started_at <= $3
		  AND event.service_id IS NOT NULL
		GROUP BY event.service_id, service.service_name, service.service_slug
		ORDER BY COUNT(*) DESC, 2, 1
		LIMIT $4`, accountID, startDate, endDate, workspaceExecutionBreakdownLimit)
	if err != nil {
		return nil, fmt.Errorf("list workspace execution service breakdown: %w", err)
	}
	defer rows.Close()
	items := make([]models.EngineExecutionBreakdown, 0, workspaceExecutionBreakdownLimit)
	for rows.Next() {
		var item models.EngineExecutionBreakdown
		if err := rows.Scan(&item.Key, &item.Label, &item.TotalCalls, &item.InboundCalls, &item.FailedCalls, &item.P95LatencyMs); err != nil {
			return nil, fmt.Errorf("scan workspace execution service breakdown: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
