package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

// TestPostgresStore_WorkspaceExecutionPolicyOverride verifies the resolution
// precedence this feature exists for: a version-tier override wins over a
// service-default override, a service-default override is the fallback for
// any version without its own row, resetting one tier leaves the other
// tier untouched, and upserting the same tier twice updates in place rather
// than creating a duplicate row (enforced by the schema's partial unique
// indexes -- this test is the behavioral proof those indexes, and the
// ON CONFLICT branching in UpsertWorkspaceExecutionPolicyOverride, actually
// work end to end against real Postgres).
func TestPostgresStore_WorkspaceExecutionPolicyOverride(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping workspace execution policy override test: DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	defer pool.Close()

	base := NewPostgresStore(pool)
	s, ok := base.(WorkspaceExecutionPolicyStore)
	if !ok {
		t.Fatal("store does not implement workspace execution policy store")
	}
	serviceID := uuid.New()
	versionID := uuid.New()
	otherVersionID := uuid.New()
	serviceTimeoutMs := 45000
	versionTimeoutMs := 5000
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_workspace_execution_policies WHERE service_id = $1`, serviceID)
	})

	// Service-default tier: rate limit only.
	serviceDefault, err := s.UpsertWorkspaceExecutionPolicyOverride(ctx, WorkspaceExecutionPolicyOverride{
		ServiceID: serviceID,
		RateLimit: testWorkspaceRateLimit(5),
		TimeoutMs: &serviceTimeoutMs,
	})
	if err != nil {
		t.Fatalf("upsert service-default override: %v", err)
	}
	if serviceDefault.ServiceVersionID != nil {
		t.Fatalf("service-default override should have nil ServiceVersionID, got %v", serviceDefault.ServiceVersionID)
	}
	if serviceDefault.RetryConfig != nil {
		t.Fatalf("unset RetryConfig should decode as nil, got %#v", serviceDefault.RetryConfig)
	}

	// A version with no override of its own falls back to the service default.
	effective, err := s.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, otherVersionID)
	if err != nil {
		t.Fatalf("get effective (fallback to service-default): %v", err)
	}
	if effective == nil || workspaceRateLimitValue(effective.RateLimit) != 5 {
		t.Fatalf("expected service-default fallback, got %#v", effective)
	}
	if effective.TimeoutMs == nil || *effective.TimeoutMs != serviceTimeoutMs {
		t.Fatalf("expected service-default timeout fallback, got %v", effective.TimeoutMs)
	}

	// Version-tier override for versionID: retry config only, no rate limit --
	// tiers are whole distinct rows, not merged field-by-field with each other
	// (only override-vs-snapshot merges per field, at read time in cache.go).
	versionOverride, err := s.UpsertWorkspaceExecutionPolicyOverride(ctx, WorkspaceExecutionPolicyOverride{
		ServiceID:        serviceID,
		ServiceVersionID: &versionID,
		RetryConfig:      &fusedobject.RetryConfig{Strategy: "exponential", MaxRetries: 3, BackoffMs: 200},
		TimeoutMs:        &versionTimeoutMs,
	})
	if err != nil {
		t.Fatalf("upsert version-tier override: %v", err)
	}
	if versionOverride.ServiceVersionID == nil || *versionOverride.ServiceVersionID != versionID {
		t.Fatalf("version-tier override ServiceVersionID = %v, want %s", versionOverride.ServiceVersionID, versionID)
	}
	if versionOverride.RateLimit != nil {
		t.Fatalf("version-tier row should not inherit the service-default's rate_limit, got %#v", versionOverride.RateLimit)
	}
	exactStore, ok := base.(WorkspaceExecutionPolicyExactBatchStore)
	if !ok {
		t.Fatal("store does not implement exact workspace execution policy batches")
	}
	serviceRef := WorkspaceExecutionPolicyRef{ServiceID: serviceID}
	versionRef := WorkspaceExecutionPolicyRef{ServiceID: serviceID, ServiceVersionID: versionID}
	missingRef := WorkspaceExecutionPolicyRef{ServiceID: serviceID, ServiceVersionID: otherVersionID}
	exact, err := exactStore.GetWorkspaceExecutionPolicyOverrides(ctx, []WorkspaceExecutionPolicyRef{serviceRef, versionRef, missingRef})
	if err != nil {
		t.Fatalf("get exact override batch: %v", err)
	}
	if exact[serviceRef] == nil || exact[versionRef] == nil || exact[missingRef] != nil {
		t.Fatalf("exact override tiers = %#v", exact)
	}

	// The version with its own row now resolves to the version-tier row, not
	// the service default.
	effective, err = s.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, versionID)
	if err != nil {
		t.Fatalf("get effective (version-tier wins): %v", err)
	}
	if effective == nil || effective.RetryConfig == nil || effective.RetryConfig.MaxRetries != 3 {
		t.Fatalf("expected version-tier override to win, got %#v", effective)
	}
	if effective.TimeoutMs == nil || *effective.TimeoutMs != versionTimeoutMs {
		t.Fatalf("expected version-tier timeout to win, got %v", effective.TimeoutMs)
	}
	if effective.RateLimit != nil {
		t.Fatalf("version-tier row should not carry the service-default's rate_limit, got %#v", effective.RateLimit)
	}

	// The other version is unaffected and still falls back to service-default.
	effective, err = s.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, otherVersionID)
	if err != nil {
		t.Fatalf("get effective (unaffected version): %v", err)
	}
	if effective == nil || workspaceRateLimitValue(effective.RateLimit) != 5 {
		t.Fatalf("expected unaffected version to still see service-default, got %#v", effective)
	}

	// Re-upserting the same tier updates in place (same row), not a duplicate.
	_, err = s.UpsertWorkspaceExecutionPolicyOverride(ctx, WorkspaceExecutionPolicyOverride{
		ServiceID:        serviceID,
		ServiceVersionID: &versionID,
		RetryConfig:      &fusedobject.RetryConfig{Strategy: "exponential", MaxRetries: 7, BackoffMs: 500},
	})
	if err != nil {
		t.Fatalf("re-upsert version-tier override: %v", err)
	}
	var versionTierRowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fused_workspace_execution_policies WHERE service_version_id = $1`, versionID).Scan(&versionTierRowCount); err != nil {
		t.Fatalf("count version-tier rows: %v", err)
	}
	if versionTierRowCount != 1 {
		t.Fatalf("expected exactly one version-tier row after re-upsert, found %d", versionTierRowCount)
	}
	effective, err = s.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, versionID)
	if err != nil {
		t.Fatalf("get effective (after re-upsert): %v", err)
	}
	if effective.RetryConfig.MaxRetries != 7 {
		t.Fatalf("re-upsert did not take effect, got MaxRetries=%d", effective.RetryConfig.MaxRetries)
	}

	// Resetting the version tier leaves the service-default tier untouched.
	if err := s.ResetWorkspaceExecutionPolicyOverride(ctx, serviceID, &versionID); err != nil {
		t.Fatalf("reset version-tier override: %v", err)
	}
	effective, err = s.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, versionID)
	if err != nil {
		t.Fatalf("get effective (after version-tier reset): %v", err)
	}
	if effective == nil || workspaceRateLimitValue(effective.RateLimit) != 5 {
		t.Fatalf("expected fallback to service-default after version-tier reset, got %#v", effective)
	}

	// Resetting the service-default tier leaves nothing behind.
	if err := s.ResetWorkspaceExecutionPolicyOverride(ctx, serviceID, nil); err != nil {
		t.Fatalf("reset service-default override: %v", err)
	}
	effective, err = s.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, versionID)
	if err != nil {
		t.Fatalf("get effective (after full reset): %v", err)
	}
	if effective != nil {
		t.Fatalf("expected no override after full reset, got %#v", effective)
	}
}

func testWorkspaceRateLimit(limit int64) *fusedobject.RateLimitConfig {
	return &fusedobject.RateLimitConfig{Version: 2, Policies: []ratelimitpolicy.Policy{{
		Name: "requests", Unit: "requests", Scope: "service_version", DefaultCost: 1,
		OperationCosts: map[string]int64{}, Algorithm: "fixed_window",
		FixedWindow: &ratelimitpolicy.FixedWindow{Limit: limit, DurationMS: 1_000},
	}}}
}

func workspaceRateLimitValue(config *fusedobject.RateLimitConfig) int64 {
	if config == nil || len(config.Policies) == 0 || config.Policies[0].FixedWindow == nil {
		return 0
	}
	return config.Policies[0].FixedWindow.Limit
}
