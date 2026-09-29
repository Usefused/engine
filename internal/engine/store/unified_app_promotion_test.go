package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// assertUnifiedAppTrafficPromotion exercises real rollback, stale-write protection, and readiness using retained versions.
func assertUnifiedAppTrafficPromotion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, familyID, firstID, secondID uuid.UUID) {
	t.Helper()
	repository := NewPostgresStore(pool).(UnifiedAppPromotionStore)
	// Start by selecting version two so the next operation must really restore version one.
	if err := repository.PromoteUnifiedAppVersion(ctx, familyID, secondID, firstID); err != nil {
		t.Fatal(err)
	}
	// The second deployment is the reviewed predecessor when restoring version one.
	if err := repository.PromoteUnifiedAppVersion(ctx, familyID, firstID, secondID); err != nil {
		t.Fatal(err)
	}
	target, err := repository.UnifiedAppTrafficTarget(ctx, familyID)
	if err != nil || target != firstID {
		t.Fatalf("restored target=%s err=%v", target, err)
	}
	// A retry of an acknowledged destination is idempotent even with its original precondition.
	if err := repository.PromoteUnifiedAppVersion(ctx, familyID, firstID, secondID); err != nil {
		t.Fatal(err)
	}
	// Another target cannot overwrite the promotion using stale state.
	if err := repository.PromoteUnifiedAppVersion(ctx, familyID, secondID, secondID); !errors.Is(err, ErrUnifiedAppTrafficChanged) {
		t.Fatalf("stale promotion: %v", err)
	}
	// Unknown versions leave the current destination untouched.
	if err := repository.PromoteUnifiedAppVersion(ctx, familyID, uuid.New(), firstID); !errors.Is(err, ErrUnifiedAppBundleNotFound) {
		t.Fatalf("missing promotion: %v", err)
	}
	// A wrong family must not adopt a version from this family.
	if err := repository.PromoteUnifiedAppVersion(ctx, uuid.New(), firstID, uuid.Nil); !errors.Is(err, ErrAppFamilyNotFound) {
		t.Fatalf("foreign family: %v", err)
	}
	target, err = repository.UnifiedAppTrafficTarget(ctx, familyID)
	if err != nil || target != firstID {
		t.Fatalf("failed promotion changed target=%s err=%v", target, err)
	}
	// Stable and pinned MCP routing must agree with the restored serving pointer.
	routes := NewPostgresStore(pool).(interface {
		ResolveMCPRoute(context.Context, uuid.UUID) (*MCPRouteTarget, error)
	})
	assertUnifiedAppPromotedRoutes(t, ctx, routes, familyID, secondID, firstID)
	// Corrupted retained code must not displace the current healthy version.
	var saved string
	if err := pool.QueryRow(ctx, `SELECT bundle_js FROM fused_unified_app_bundles WHERE app_id=$1`, secondID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_unified_app_bundles SET bundle_js='corrupt' WHERE app_id=$1`, secondID); err != nil {
		t.Fatal(err)
	}
	if err := repository.PromoteUnifiedAppVersion(ctx, familyID, secondID, firstID); !errors.Is(err, ErrUnifiedAppBundleDigestMismatch) {
		t.Fatalf("corrupt promotion: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_unified_app_bundles SET bundle_js=$2 WHERE app_id=$1`, secondID, saved); err != nil {
		t.Fatal(err)
	}
}
