package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/ratelimitcoordinator"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProviderRateLimitJetStreamProjectsEventuallyToPostgres(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository := store.NewPostgresStore(pool)
	projectionStore := repository.(interface {
		BatchUpsertProviderRateLimitStates(context.Context, []ratelimitpolicy.StateEnvelope) error
	})
	recoveryStore := repository.(ratelimitcoordinator.RecoveryStore)

	client := projectionTestNATSClient(t)
	kv, err := client.InitProviderRateLimitBucket()
	if err != nil {
		t.Fatal(err)
	}
	projectionWorker, err := StartProviderRateLimitProjectionWorker(ctx, projectionStore, client)
	if err != nil {
		t.Fatal(err)
	}
	defer projectionWorker.Stop(context.Background())
	coordinator, err := ratelimitcoordinator.New(kv, recoveryStore)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	request := integrationProjectionRequest()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_provider_rate_limit_states WHERE account_id = $1`, request.AccountID)
	})
	decision, err := coordinator.AcquireProviderRateLimit(ctx, request)
	if err != nil || !decision.Allowed {
		t.Fatalf("acquire = %+v, %v", decision, err)
	}
	assertEventuallyProjectedUsage(t, ctx, pool, request, 8)
}

func integrationProjectionRequest() ratelimitpolicy.AcquireRequest {
	return ratelimitpolicy.AcquireRequest{
		AccountID: uuid.New(), ServiceVersionID: uuid.New(),
		Policies: []ratelimitpolicy.ResolvedPolicy{{
			Name: "minute", Unit: "request", ScopeKind: "connection", ScopeID: uuid.NewString(),
			Cost: 1, Algorithm: "fixed_window", ConfigHash: "integration-config",
			Limit: 10, DurationMs: time.Minute.Milliseconds(),
		}},
	}
}

func assertEventuallyProjectedUsage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, request ratelimitpolicy.AcquireRequest, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var used int64
		err := pool.QueryRow(ctx, `
			SELECT fixed_window_used FROM fused_provider_rate_limit_states
			WHERE account_id=$1 AND service_version_id=$2 AND policy_name=$3`,
			request.AccountID, request.ServiceVersionID, request.Policies[0].Name,
		).Scan(&used)
		if err == nil && used == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("provider rate-limit state was not projected with usage %d", want)
}
