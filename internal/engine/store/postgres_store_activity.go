package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

func (s *postgresStore) GetWorkspaceExecutionAnalytics(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time) (models.WorkspaceExecutionAnalytics, error) {
	var analytics models.WorkspaceExecutionAnalytics
	if err := s.scanWorkspaceExecutionSummary(ctx, accountID, startDate, endDate, &analytics); err != nil {
		return models.WorkspaceExecutionAnalytics{}, err
	}
	byService, err := s.listWorkspaceExecutionBreakdown(ctx, accountID, startDate, endDate, "service")
	if err != nil {
		return models.WorkspaceExecutionAnalytics{}, err
	}
	byTransport, err := s.listWorkspaceExecutionBreakdown(ctx, accountID, startDate, endDate, "transport")
	if err != nil {
		return models.WorkspaceExecutionAnalytics{}, err
	}
	failures, err := s.listWorkspaceExecutionFailures(ctx, accountID, startDate, endDate, 10)
	if err != nil {
		return models.WorkspaceExecutionAnalytics{}, err
	}
	analytics.ByService, analytics.ByTransport, analytics.RecentFailures = byService, byTransport, failures
	return analytics, nil
}

func (s *postgresStore) scanWorkspaceExecutionSummary(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time, analytics *models.WorkspaceExecutionAnalytics) error {
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE event.status = 'success'),
		       COUNT(*) FILTER (WHERE event.status = 'failed'), COALESCE(AVG(event.latency_ms), 0),
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY event.latency_ms), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY event.latency_ms), 0)
		FROM fused_engine_execution_events event
		JOIN fused_workspace_services service ON service.service_id = event.service_id
		WHERE event.account_id = $1 AND event.started_at >= $2 AND event.started_at <= $3`, accountID, startDate, endDate).Scan(
		&analytics.TotalCalls, &analytics.SuccessfulCalls, &analytics.FailedCalls,
		&analytics.AverageLatencyMs, &analytics.MedianLatencyMs, &analytics.P95LatencyMs,
	)
	if err != nil {
		return fmt.Errorf("get workspace execution summary: %w", err)
	}
	return nil
}

func (s *postgresStore) listWorkspaceExecutionBreakdown(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time, dimension string) ([]models.EngineExecutionBreakdown, error) {
	keyExpression, labelExpression := "event.transport", "UPPER(event.transport)"
	if dimension == "service" {
		keyExpression, labelExpression = "event.service_id::text", "COALESCE(NULLIF(service.service_name, ''), NULLIF(service.service_slug, ''), event.service_id::text)"
	}
	query := `SELECT ` + keyExpression + `, ` + labelExpression + `, COUNT(*),
		COUNT(*) FILTER (WHERE event.status = 'failed'),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY event.latency_ms), 0)
		FROM fused_engine_execution_events event
		JOIN fused_workspace_services service ON service.service_id = event.service_id
		WHERE event.account_id = $1 AND event.started_at >= $2 AND event.started_at <= $3
		GROUP BY 1, 2 ORDER BY COUNT(*) DESC, 2`
	rows, err := s.db.Query(ctx, query, accountID, startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("list workspace execution %s breakdown: %w", dimension, err)
	}
	defer rows.Close()
	items := make([]models.EngineExecutionBreakdown, 0)
	for rows.Next() {
		var item models.EngineExecutionBreakdown
		if err := rows.Scan(&item.Key, &item.Label, &item.TotalCalls, &item.FailedCalls, &item.P95LatencyMs); err != nil {
			return nil, fmt.Errorf("scan workspace execution %s breakdown: %w", dimension, err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *postgresStore) listWorkspaceExecutionFailures(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time, limit int) ([]models.EngineExecutionFailure, error) {
	rows, err := s.db.Query(ctx, `
		SELECT event.id, event.service_id,
		       COALESCE(NULLIF(service.service_name, ''), NULLIF(service.service_slug, ''), event.service_id::text),
		       COALESCE(NULLIF(event.endpoint_name, ''), NULLIF(event.event_name, ''), 'Unknown operation'),
		       event.transport, COALESCE(event.failure_category, ''), COALESCE(event.failure_code, ''),
		       COALESCE(event.failure_reason, ''), event.latency_ms, event.started_at
		FROM fused_engine_execution_events event
		JOIN fused_workspace_services service ON service.service_id = event.service_id
		WHERE event.account_id = $1 AND event.status = 'failed'
		  AND event.started_at >= $2 AND event.started_at <= $3
		ORDER BY event.started_at DESC LIMIT $4`, accountID, startDate, endDate, limit)
	if err != nil {
		return nil, fmt.Errorf("list workspace execution failures: %w", err)
	}
	defer rows.Close()
	items := make([]models.EngineExecutionFailure, 0, limit)
	for rows.Next() {
		var item models.EngineExecutionFailure
		if err := rows.Scan(
			&item.ID, &item.ServiceID, &item.ServiceName, &item.Operation, &item.Transport,
			&item.FailureCategory, &item.FailureCode, &item.FailureReason, &item.LatencyMs, &item.StartedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workspace execution failure: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
