package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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

func seedPublicInsightExecutionEvents(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, accountID, serviceID, versionID, operationID uuid.UUID, startedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_engine_execution_events (
			account_id, transport, direction, service_id, service_version_id, operation_id,
			endpoint_name, provider_status_class, status, latency_ms, provider_latency_ms,
			attempt_count, started_at, ended_at
		) VALUES
		($1, 'sdk', 'outbound', $2, $3, $4, 'issues.list', '2xx', 'success', 20, 15, 1, $5, $5),
		($1, 'mcp', 'outbound', $2, $3, $4, 'issues.list', '5xx', 'failed', 80, 70, 2, $5, $5)`,
		accountID, serviceID, versionID.String(), operationID, startedAt); err != nil {
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
