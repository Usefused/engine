package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProviderRateLimitSQLIsSetBasedAndAtomic(t *testing.T) {
	for _, expected := range []string{
		"jsonb_to_recordset", "ORDER BY name, scope_kind, scope_id", "ON CONFLICT",
	} {
		if !strings.Contains(initializeProviderRateLimitSQL, expected) {
			t.Fatalf("set-based initialization SQL missing %q", expected)
		}
	}
	for _, expected := range []string{
		"jsonb_to_recordset", "ORDER BY state.policy_name, state.scope_kind, state.scope_id", "FOR UPDATE OF state", "BOOL_AND(row_allowed)",
		"WHERE decision.allowed", "CROSS JOIN (SELECT COUNT(*) FROM consumed)",
	} {
		if !strings.Contains(acquireProviderRateLimitSQL, expected) {
			t.Fatalf("atomic acquisition SQL missing %q", expected)
		}
	}
	for _, expected := range []string{
		"ORDER BY state.policy_name, state.scope_kind, state.scope_id", "FOR UPDATE OF state",
		"state.fixed_window_used", "state.tokens", "cooldown_until = GREATEST",
	} {
		if !strings.Contains(syncProviderRateLimitSQL, expected) {
			t.Fatalf("clamp-only sync SQL missing %q", expected)
		}
	}
}

func TestProviderRateLimitAtomicAcrossStoreInstances(t *testing.T) {
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
	first := NewPostgresStore(pool).(ProviderRateLimitStore)
	second := NewPostgresStore(pool).(ProviderRateLimitStore)
	accountID, versionID := uuid.New(), uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_provider_rate_limit_states WHERE account_id = $1`, accountID)
	})
	request := providerRateLimitTestRequest(accountID, versionID, 7, "requests")
	var allowed atomic.Int64
	var wg sync.WaitGroup
	errors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			candidate := first
			if index%2 == 1 {
				candidate = second
			}
			decision, acquireErr := candidate.AcquireProviderRateLimit(ctx, request)
			if acquireErr != nil {
				errors <- acquireErr
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for acquireErr := range errors {
		t.Fatal(acquireErr)
	}
	if got := allowed.Load(); got != 7 {
		t.Fatalf("allowed = %d, want exact shared limit 7", got)
	}
}

func TestProviderRateLimitANDDoesNotPartiallyConsume(t *testing.T) {
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
	coordinator := NewPostgresStore(pool).(ProviderRateLimitStore)
	accountID, versionID := uuid.New(), uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_provider_rate_limit_states WHERE account_id = $1`, accountID)
	})
	request := providerRateLimitTestRequest(accountID, versionID, 10, "wide")
	request.Policies = append(request.Policies, providerRateLimitTestRequest(accountID, versionID, 1, "narrow").Policies[0])
	first, err := coordinator.AcquireProviderRateLimit(ctx, request)
	if err != nil || !first.Allowed {
		t.Fatalf("first decision=%#v err=%v", first, err)
	}
	second, err := coordinator.AcquireProviderRateLimit(ctx, request)
	if err != nil || second.Allowed {
		t.Fatalf("second decision=%#v err=%v", second, err)
	}
	var used int64
	if err := pool.QueryRow(ctx, `SELECT fixed_window_used FROM fused_provider_rate_limit_states WHERE account_id=$1 AND service_version_id=$2 AND policy_name='wide'`, accountID, versionID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatalf("wide policy used=%d, want 1 after denied AND acquisition", used)
	}
}

func TestProviderRateLimitTokenBucketAndResponseClamp(t *testing.T) {
	ctx, pool, coordinator := providerRateLimitIntegrationStore(t)
	accountID, versionID := uuid.New(), uuid.New()
	cleanupProviderRateLimitAccount(t, pool, accountID)
	request := providerTokenBucketTestRequest(accountID, versionID, 3, "tokens-v1")
	for attempt := 0; attempt < 3; attempt++ {
		decision, err := coordinator.AcquireProviderRateLimit(ctx, request)
		if err != nil || !decision.Allowed {
			t.Fatalf("token attempt %d decision=%#v err=%v", attempt, decision, err)
		}
	}
	decision, err := coordinator.AcquireProviderRateLimit(ctx, request)
	if err != nil || decision.Allowed {
		t.Fatalf("exhausted token decision=%#v err=%v", decision, err)
	}
	remaining := int64(0)
	if err := coordinator.SyncProviderRateLimit(ctx, ratelimitpolicy.SyncRequest{
		AccountID: accountID, ServiceVersionID: versionID,
		Observations: []ratelimitpolicy.ResponseObservation{{
			PolicyName: "token", ScopeKind: "service_version", ScopeID: versionID,
			Algorithm: "token_bucket", LocalLimit: 3, Remaining: &remaining,
		}},
	}); err != nil {
		t.Fatalf("sync response clamp: %v", err)
	}
}

func TestProviderRateLimitConfigChangeResetsState(t *testing.T) {
	ctx, pool, coordinator := providerRateLimitIntegrationStore(t)
	accountID, versionID := uuid.New(), uuid.New()
	cleanupProviderRateLimitAccount(t, pool, accountID)
	request := providerRateLimitTestRequest(accountID, versionID, 1, "fixed")
	assertProviderRateLimitAllowed(t, ctx, coordinator, request, true)
	assertProviderRateLimitAllowed(t, ctx, coordinator, request, false)
	request.Policies[0].ConfigHash = "fixed-v2"
	request.Policies[0].Limit = 2
	assertProviderRateLimitAllowed(t, ctx, coordinator, request, true)
}

func providerRateLimitIntegrationStore(t *testing.T) (context.Context, *pgxpool.Pool, ProviderRateLimitStore) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, NewPostgresStore(pool).(ProviderRateLimitStore)
}

func cleanupProviderRateLimitAccount(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_provider_rate_limit_states WHERE account_id = $1`, accountID)
	})
}

func providerTokenBucketTestRequest(accountID, versionID uuid.UUID, capacity int64, configHash string) ratelimitpolicy.AcquireRequest {
	return ratelimitpolicy.AcquireRequest{AccountID: accountID, ServiceVersionID: versionID, Policies: []ratelimitpolicy.ResolvedPolicy{{
		Name: "token", Unit: "points", ScopeKind: "service_version", ScopeID: versionID.String(), Cost: 1,
		Algorithm: "token_bucket", ConfigHash: configHash, Capacity: capacity, RefillUnits: 1, RefillIntervalMS: 60_000,
	}}}
}

func assertProviderRateLimitAllowed(t *testing.T, ctx context.Context, coordinator ProviderRateLimitStore, request ratelimitpolicy.AcquireRequest, want bool) {
	t.Helper()
	decision, err := coordinator.AcquireProviderRateLimit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed != want {
		t.Fatalf("allowed=%t want=%t decision=%#v", decision.Allowed, want, decision)
	}
}

func providerRateLimitTestRequest(accountID, versionID uuid.UUID, limit int64, name string) ratelimitpolicy.AcquireRequest {
	return ratelimitpolicy.AcquireRequest{AccountID: accountID, ServiceVersionID: versionID, Policies: []ratelimitpolicy.ResolvedPolicy{{
		Name: name, Unit: "requests", ScopeKind: "service_version", ScopeID: versionID.String(), Cost: 1,
		Algorithm: "fixed_window", ConfigHash: name, Limit: limit, DurationMS: 60_000,
	}}}
}
