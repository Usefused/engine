package store

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestMCPStableRoutePromotionAndPinnedResolution verifies apply promotion,
// immutable pinning, rollback promotion, and hard-deactivation behavior.
func TestMCPStableRoutePromotionAndPinnedResolution(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeMCP)
	fixture.params.TokenHash = "stable-route-" + uuid.NewString()
	firstResult, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	// The first apply establishes both immutable runtime and initial family target.
	if err != nil {
		t.Fatalf("apply first MCP version: %v", err)
	}
	runtimeStore := NewPostgresStore(fixture.pool)
	first, err := runtimeStore.GetApp(fixture.ctx, firstResult.AppID)
	// Reusing the persisted row keeps the second version focused on route behavior.
	if err != nil {
		t.Fatalf("load first MCP version: %v", err)
	}

	second := *first
	second.AppID = uuid.New()
	second.Version = uuid.NewString()
	second.ConfigKey = "mcp:concurrent:" + second.Version
	second.SourceHash = "source-" + second.Version
	second.ExpectedFamilyKind = AppKindMCP
	// Publishing a distinct immutable version is the upgrade boundary under test.
	if _, created, err := runtimeStore.PublishAppVersion(fixture.ctx, second); err != nil || !created {
		t.Fatalf("publish second MCP version = created %t, error %v", created, err)
	}

	stable, err := runtimeStore.ResolveMCPRoute(fixture.ctx, firstResult.AppFamilyID)
	// Family routing must advance to the exact version most recently applied.
	if err != nil || !stable.Stable || stable.AppID != second.AppID {
		t.Fatalf("stable route after upgrade = %#v, %v", stable, err)
	}
	pinned, err := runtimeStore.ResolveMCPRoute(fixture.ctx, first.AppID)
	// Version IDs remain immutable escape hatches after the family is promoted.
	if err != nil || pinned.Stable || pinned.AppID != first.AppID {
		t.Fatalf("pinned route after upgrade = %#v, %v", pinned, err)
	}

	first.ExpectedFamilyKind = AppKindMCP
	// Reapplying identical content is a no-op publication but an explicit rollback promotion.
	if _, created, err := runtimeStore.PublishAppVersion(fixture.ctx, *first); err != nil || created {
		t.Fatalf("reapply first MCP version = created %t, error %v", created, err)
	}
	rolledBack, err := runtimeStore.ResolveMCPRoute(fixture.ctx, firstResult.AppFamilyID)
	// The stable URL must now select the explicitly reapplied older version.
	if err != nil || rolledBack.AppID != first.AppID {
		t.Fatalf("stable route after rollback = %#v, %v", rolledBack, err)
	}

	// Hard deactivation must clear the FK-backed target atomically with deletion.
	if err := runtimeStore.DeactivateAppVersion(fixture.ctx, first.AppID, uuid.Nil); err != nil {
		t.Fatalf("deactivate promoted MCP version: %v", err)
	}
	// A removed promoted version must leave the stable URL unavailable instead
	// of silently choosing a sibling the user did not promote.
	if _, err := runtimeStore.ResolveMCPRoute(fixture.ctx, firstResult.AppFamilyID); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("stable route after deactivation error = %v, want ErrAppNotFound", err)
	}
	restartedPool, err := db.InitEnginePostgres(fixture.ctx, fixture.pool.Config().ConnString())
	// Startup reconciliation must remember the explicit empty target rather than
	// interpreting it as an old database that still needs its initial backfill.
	if err != nil {
		t.Fatalf("reinitialize Engine schema: %v", err)
	}
	t.Cleanup(restartedPool.Close)
	if _, err := NewPostgresStore(restartedPool).ResolveMCPRoute(fixture.ctx, firstResult.AppFamilyID); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("stable route after schema restart error = %v, want ErrAppNotFound", err)
	}
	// Deactivating the promoted version must not remove a runnable pinned sibling.
	if pinned, err := runtimeStore.ResolveMCPRoute(fixture.ctx, second.AppID); err != nil || pinned.AppID != second.AppID {
		t.Fatalf("remaining pinned version = %#v, %v", pinned, err)
	}
}
