package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WebhookActivityFilter keeps exact registration scope identical for history, totals, and latency aggregates.
type WebhookActivityFilter struct {
	AccountID, ServiceID uuid.UUID
	RegistrationID       *uuid.UUID
	EventName, Status    string
	Limit, Offset        int
	StartDate, EndDate   *time.Time
}

// WebhookActivityReader extends existing stores without allowing a service-wide fallback for registration queries.
type WebhookActivityReader interface {
	ListWebhookActivity(context.Context, WebhookActivityFilter) ([]models.WebhookEvent, int64, error)
	GetWebhookActivityAnalytics(context.Context, WebhookActivityFilter) (models.WebhookAnalytics, error)
}

// webhookActivityWhere binds tenant and service even when an exact registration ID is supplied.
func webhookActivityWhere(filter WebhookActivityFilter) (string, []any) {
	where := "WHERE account_id = $1 AND service_id = $2 AND transport = 'webhook'"
	args := []any{filter.AccountID, filter.ServiceID}
	// Optional predicates only narrow the mandatory account and service boundary.
	for _, predicate := range []struct {
		sql     string
		value   any
		enabled bool
	}{
		{"webhook_id =", filter.RegistrationID, filter.RegistrationID != nil},
		{"event_name =", filter.EventName, filter.EventName != ""},
		{"started_at >=", filter.StartDate, filter.StartDate != nil},
		{"started_at <=", filter.EndDate, filter.EndDate != nil},
	} {
		// An absent filter never creates a NULL comparison or replaces the mandatory scope.
		if predicate.enabled {
			args = append(args, predicate.value)
			where += fmt.Sprintf(" AND %s $%d", predicate.sql, len(args))
		}
	}
	// Rejections are failures too; retain a separate selection for verification troubleshooting.
	switch filter.Status {
	case "failed":
		where += " AND (status = 'failed' OR delivery_status IN ('failed', 'rejected', 'nacked'))"
	case "rejected":
		where += " AND delivery_status = 'rejected'"
	case "success":
		where += " AND status = 'success' AND delivery_status NOT IN ('failed', 'rejected', 'nacked')"
	}
	return where, args
}

// ListWebhookEventsByService preserves service Activity while sharing canonical registration query semantics.
func (s *postgresStore) ListWebhookEventsByService(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, limit, offset int, startDate, endDate *time.Time) ([]models.WebhookEvent, int64, error) {
	return s.ListWebhookActivity(ctx, WebhookActivityFilter{AccountID: accountID, ServiceID: serviceID, EventName: eventName, Limit: limit, Offset: offset, StartDate: startDate, EndDate: endDate})
}

// ListWebhookActivity filters in SQL before both counting and paging; sibling registrations never enter a page.
func (s *postgresStore) ListWebhookActivity(ctx context.Context, filter WebhookActivityFilter) ([]models.WebhookEvent, int64, error) {
	where, args := webhookActivityWhere(filter)
	var total int64
	// Preserve totals for an offset beyond the final page and fail explicitly on database errors.
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM fused_engine_execution_events "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT id, account_id, service_id, COALESCE(external_id, '') AS msg_id, COALESCE(event_name, '') AS event_type,
		webhook_id AS registration_id, service_version_id, COALESCE(trace_id, '') AS trace_id, COALESCE(span_id, '') AS span_id,
		status AS execution_status, COALESCE(failure_category, '') AS failure_category, COALESCE(failure_code, '') AS failure_code,
		attempt_count, ended_at, created_at AS recorded_at,
		COALESCE(failure_reason, '') AS error_reason, app_id AS sdk_record_id,
		COALESCE(verification_status, '') AS verification_status, COALESCE(delivery_status, '') AS delivery_status,
		COALESCE(environment, '') AS environment, latency_ms::integer AS latency_ms,
		GREATEST(attempt_count - 1, 0) AS retry_count, 0::double precision AS credits_consumed,
		request_bytes::integer AS payload_size, started_at AS created_at FROM fused_engine_execution_events ` + where +
		fmt.Sprintf(" ORDER BY started_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := s.db.Query(ctx, query, append(args, filter.Limit, filter.Offset)...)
	// An unavailable history must never be displayed as a verified empty list.
	if err != nil {
		return nil, 0, err
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[models.WebhookEvent])
	return events, total, err
}

// GetWebhookAnalytics preserves the existing service aggregate API on the shared canonical receipt source.
func (s *postgresStore) GetWebhookAnalytics(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, startDate, endDate *time.Time) (models.WebhookAnalytics, error) {
	return s.GetWebhookActivityAnalytics(ctx, WebhookActivityFilter{AccountID: accountID, ServiceID: serviceID, EventName: eventName, StartDate: startDate, EndDate: endDate})
}

// GetWebhookActivityAnalytics calculates counts and latency from the same scoped receipts shown in history.
func (s *postgresStore) GetWebhookActivityAnalytics(ctx context.Context, filter WebhookActivityFilter) (models.WebhookAnalytics, error) {
	where, args := webhookActivityWhere(filter)
	var analytics models.WebhookAnalytics
	query := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE delivery_status IN ('success', 'delivered', 'acked')),
		COUNT(*) FILTER (WHERE delivery_status = 'rejected'),
		COUNT(*) FILTER (WHERE delivery_status IN ('failed', 'nacked')),
		COALESCE(AVG(latency_ms), 0), COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM fused_engine_execution_events ` + where
	err := s.db.QueryRow(ctx, query, args...).Scan(&analytics.TotalIngested, &analytics.TotalDelivered,
		&analytics.TotalRejected, &analytics.TotalFailed, &analytics.AverageLatencyMs, &analytics.P95LatencyMs)
	return analytics, err
}

// ListWebhookActivity bypasses runtime caching so a freshly received delivery becomes visible on refresh.
func (s *cachedStore) ListWebhookActivity(ctx context.Context, filter WebhookActivityFilter) ([]models.WebhookEvent, int64, error) {
	reader, ok := s.Store.(WebhookActivityReader)
	// Unsupported delegates must never substitute a wider service query for exact-registration history.
	if !ok {
		return nil, 0, fmt.Errorf("registration activity is unavailable")
	}
	return reader.ListWebhookActivity(ctx, filter)
}

// GetWebhookActivityAnalytics delegates the same exact scope without caching stale counters.
func (s *cachedStore) GetWebhookActivityAnalytics(ctx context.Context, filter WebhookActivityFilter) (models.WebhookAnalytics, error) {
	reader, ok := s.Store.(WebhookActivityReader)
	// Missing reporting support remains explicit rather than a fabricated zero summary.
	if !ok {
		return models.WebhookAnalytics{}, fmt.Errorf("registration analytics are unavailable")
	}
	return reader.GetWebhookActivityAnalytics(ctx, filter)
}
