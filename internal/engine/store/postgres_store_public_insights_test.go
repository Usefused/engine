package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicInsightProjectionIsAtomicAndIdempotent(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)

	accountID, serviceID, versionID, operationID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	seedPublicInsightExecutionEvents(t, ctx, pool, accountID, serviceID, versionID, operationID, startedAt)
	assertPublicInsightProjection(t, ctx, repository, serviceID, operationID)
}

func TestMarkPublicServiceInsightReportResultsUsesTimestampType(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)
	acceptedID, rejectedID := uuid.New(), uuid.New()
	seedPendingPublicInsightReports(t, ctx, pool, acceptedID, rejectedID)
	markedAt := time.Now().UTC().Truncate(time.Microsecond)

	err := repository.MarkPublicServiceInsightReportResults(ctx, []models.PublicServiceInsightReportResult{
		{ReportID: acceptedID, Accepted: true},
		{ReportID: rejectedID, Accepted: false, Reason: "not_public"},
	}, markedAt)
	if err != nil {
		t.Fatalf("mark public insight reports: %v", err)
	}
	assertPublicInsightReportResults(t, ctx, pool, acceptedID, rejectedID, markedAt)
}

func seedPendingPublicInsightReports(t *testing.T, ctx context.Context, pool *pgxpool.Pool, reportIDs ...uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO fused_public_service_insight_outbox (
			report_id, service_id, service_version_id, registry_object_kind, registry_object_id,
			direction, transport, outcome, provider_status_class, bucket_start,
			call_count, total_latency_ms_sum, provider_latency_ms_sum, latency_histogram, retry_attempts_sum
		)
		SELECT report_id, gen_random_uuid(), gen_random_uuid(), 'endpoint', gen_random_uuid(),
		       'outbound', 'sdk', 'success', '2xx', $2,
		       1, 10, 8, '[1,0,0,0,0,0,0,0,0,0,0,0]'::jsonb, 1
		FROM unnest($1::uuid[]) AS report_id`, reportIDs, time.Now().UTC().Add(-2*time.Hour).Truncate(time.Hour))
	if err != nil {
		t.Fatalf("seed pending public insight reports: %v", err)
	}
}

func assertPublicInsightReportResults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acceptedID, rejectedID uuid.UUID, markedAt time.Time) {
	t.Helper()
	var acceptedState, rejectedState, rejectedCode string
	var acceptedSentAt, acceptedUpdatedAt, rejectedUpdatedAt time.Time
	var rejectedSentAt *time.Time
	var acceptedAttempts, rejectedAttempts int
	err := pool.QueryRow(ctx, `
		SELECT accepted.state, accepted.sent_at, accepted.updated_at, accepted.attempt_count,
		       rejected.state, rejected.sent_at, rejected.updated_at, rejected.attempt_count, rejected.last_error_code
		FROM fused_public_service_insight_outbox accepted
		JOIN fused_public_service_insight_outbox rejected ON rejected.report_id = $2
		WHERE accepted.report_id = $1`, acceptedID, rejectedID).Scan(
		&acceptedState, &acceptedSentAt, &acceptedUpdatedAt, &acceptedAttempts,
		&rejectedState, &rejectedSentAt, &rejectedUpdatedAt, &rejectedAttempts, &rejectedCode,
	)
	if err != nil {
		t.Fatalf("read marked public insight reports: %v", err)
	}
	if acceptedState != "sent" {
		t.Fatalf("accepted state = %s, want sent", acceptedState)
	}
	if rejectedState != "rejected" {
		t.Fatalf("rejected state = %s, want rejected", rejectedState)
	}
	if rejectedSentAt != nil {
		t.Fatalf("rejected sent_at = %v, want nil", rejectedSentAt)
	}
	if rejectedCode != "not_public" {
		t.Fatalf("rejected code = %s, want not_public", rejectedCode)
	}
	assertPublicInsightTimestamps(t, acceptedSentAt, acceptedUpdatedAt, rejectedUpdatedAt, markedAt)
	if acceptedAttempts != 1 {
		t.Fatalf("accepted attempts = %d, want 1", acceptedAttempts)
	}
	if rejectedAttempts != 1 {
		t.Fatalf("rejected attempts = %d, want 1", rejectedAttempts)
	}
}

func assertPublicInsightTimestamps(t *testing.T, acceptedSentAt, acceptedUpdatedAt, rejectedUpdatedAt, want time.Time) {
	t.Helper()
	if !acceptedSentAt.Equal(want) {
		t.Fatalf("accepted sent_at = %s, want %s", acceptedSentAt, want)
	}
	if !acceptedUpdatedAt.Equal(want) {
		t.Fatalf("accepted updated_at = %s, want %s", acceptedUpdatedAt, want)
	}
	if !rejectedUpdatedAt.Equal(want) {
		t.Fatalf("rejected updated_at = %s, want %s", rejectedUpdatedAt, want)
	}
}

func seedPublicInsightExecutionEvents(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, accountID, serviceID, versionID, operationID uuid.UUID, startedAt time.Time) {
	t.Helper()
	sdkFamilyID, mcpFamilyID, sdkAppID, mcpAppID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_engine_execution_events (
			account_id, app_family_id, app_id, app_version, transport, direction,
			service_id, service_version_id, operation_id,
			endpoint_name, provider_status_class, status, latency_ms, provider_latency_ms,
			attempt_count, started_at, ended_at
		) VALUES
		($1, $6, $8, '1.0.0', 'sdk', 'outbound', $2, $3, $4, 'issues.list', '2xx', 'success', 20, 15, 1, $5, $5),
		($1, $7, $9, '2.0.0', 'mcp', 'outbound', $2, $3, $4, 'issues.list', '5xx', 'failed', 80, 70, 2, $5, $5)`,
		accountID, serviceID, versionID.String(), operationID, startedAt, sdkFamilyID, mcpFamilyID, sdkAppID, mcpAppID); err != nil {
		t.Fatalf("seed execution events: %v", err)
	}
}

func assertPublicInsightProjection(t *testing.T, ctx context.Context, repository *postgresStore, serviceID, operationID uuid.UUID) {
	t.Helper()
	projected, err := repository.ProjectPublicServiceInsightReports(ctx, []uuid.UUID{serviceID}, time.Now().UTC().Truncate(time.Hour), 100)
	requireProjectedCount(t, projected, err, 2)
	reports, err := repository.ListPendingPublicServiceInsightReports(ctx, 10, time.Now().UTC())
	requirePendingPublicReports(t, reports, err, 2)
	for _, report := range reports {
		if report.ServiceID != serviceID || report.RegistryObjectID != operationID || len(report.LatencyHistogram) != 12 {
			t.Fatalf("unexpected report: %#v", report)
		}
	}
	projected, err = repository.ProjectPublicServiceInsightReports(ctx, []uuid.UUID{serviceID}, time.Now().UTC().Truncate(time.Hour), 100)
	requireProjectedCount(t, projected, err, 0)
}

func requireProjectedCount(t *testing.T, got int64, err error, want int64) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("projected = %d, %v; want %d", got, err, want)
	}
}

func requirePendingPublicReports(t *testing.T, reports []models.PublicServiceInsightReport, err error, want int) {
	t.Helper()
	if err != nil || len(reports) != want {
		t.Fatalf("reports = %#v, %v; want %d grouped outcomes", reports, err, want)
	}
}
