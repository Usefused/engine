package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestGetAppFamilyQuotaUsage verifies quota usage follows executable
// versions rather than retained family identity or deactivation tombstones.
func TestGetAppFamilyQuotaUsage(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Integration writes require an explicitly selected PostgreSQL database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Canonical schema initialization must succeed before lifecycle rows are seeded.
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(pool.Close)
	repository := NewPostgresStore(pool)
	accountID, otherAccountID := uuid.New(), uuid.New()
	teamID := seedAppOwnerTeam(t, ctx, pool)
	activeFamilyID, deprecatedFamilyID := uuid.New(), uuid.New()
	buildingFamilyID, retainedFamilyID := uuid.New(), uuid.New()
	mcpFamilyID, otherFamilyID := uuid.New(), uuid.New()
	activeAppID, activeSiblingAppID, deprecatedAppID := uuid.New(), uuid.New(), uuid.New()
	buildingAppID, mcpAppID, otherAppID := uuid.New(), uuid.New(), uuid.New()
	familyIDs := []uuid.UUID{activeFamilyID, deprecatedFamilyID, buildingFamilyID, retainedFamilyID, mcpFamilyID, otherFamilyID}
	t.Cleanup(func() {
		// Tombstones outlive app rows, so cleanup removes history before family identity.
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_tombstones WHERE app_family_id = ANY($1)`, familyIDs)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_family_id = ANY($1)`, familyIDs)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id = ANY($1)`, familyIDs)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_teams WHERE id = $1`, teamID)
	})

	_, err = pool.Exec(ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES
			($1, $7, 'sdk', 'active-sdk', 'Active SDK', 'typescript', $9),
			($2, $7, 'sdk', 'deprecated-sdk', 'Deprecated SDK', 'typescript', $9),
			($3, $7, 'sdk', 'building-sdk', 'Building SDK', 'typescript', $9),
			($4, $7, 'sdk', 'retained-sdk', 'Retained SDK', 'typescript', $9),
			($5, $7, 'mcp', 'active-mcp', 'Active MCP', NULL, $9),
			($6, $8, 'sdk', 'other-sdk', 'Other SDK', 'typescript', $9)
	`, activeFamilyID, deprecatedFamilyID, buildingFamilyID, retainedFamilyID, mcpFamilyID, otherFamilyID, accountID, otherAccountID, teamID)
	// Family fixtures must exist before their exact version rows can satisfy the composite foreign key.
	if err != nil {
		t.Fatalf("seed app families: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key, status, sdk_generation_job_id, sdk_generation_status)
		VALUES
			($1, $7, $12, '1.0.0', $14, 'active', NULL, NULL),
			($2, $7, $12, '2.0.0', $15, 'deprecated', NULL, NULL),
			($3, $8, $12, '1.0.0', $16, 'deprecated', NULL, NULL),
			($4, $9, $12, '1.0.0', $17, 'building', 'job-building', 'pending'),
			($5, $10, $12, '1.0.0', $18, 'active', NULL, NULL),
			($6, $11, $13, '1.0.0', $19, 'active', NULL, NULL)
	`, activeAppID, activeSiblingAppID, deprecatedAppID, buildingAppID, mcpAppID, otherAppID,
		activeFamilyID, deprecatedFamilyID, buildingFamilyID, mcpFamilyID, otherFamilyID,
		accountID, otherAccountID,
		"sdk:active:"+activeAppID.String(), "sdk:active-sibling:"+activeSiblingAppID.String(), "sdk:deprecated:"+deprecatedAppID.String(), "sdk:building:"+buildingAppID.String(), "mcp:active:"+mcpAppID.String(), "sdk:other:"+otherAppID.String())
	// Runnable, deprecated, and building states are deliberately mixed in one bounded fixture.
	if err != nil {
		t.Fatalf("seed app versions: %v", err)
	}

	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, AppKindSDK.String(), "active-sdk", 2, true)
	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, AppKindSDK.String(), "retained-sdk", 2, false)
	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, AppKindMCP.String(), "active-mcp", 1, true)
	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, "", "active-mcp", 3, true)
	assertAppFamilyQuotaUsage(t, ctx, repository, otherAccountID, AppKindSDK.String(), "other-sdk", 1, true)

	// Removing one exact version cannot free a family while a runnable sibling remains.
	if err := repository.DeactivateAppVersion(ctx, activeAppID, uuid.Nil); err != nil {
		t.Fatalf("deactivate active SDK: %v", err)
	}
	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, AppKindSDK.String(), "active-sdk", 2, true)
	// Removing the last runnable sibling frees only that family's single quota unit.
	if err := repository.DeactivateAppVersion(ctx, activeSiblingAppID, uuid.Nil); err != nil {
		t.Fatalf("deactivate active SDK sibling: %v", err)
	}
	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, AppKindSDK.String(), "active-sdk", 1, false)
	// The remaining deprecated family is invokable until its own final version is hard-deactivated.
	if err := repository.DeactivateAppVersion(ctx, deprecatedAppID, uuid.Nil); err != nil {
		t.Fatalf("deactivate deprecated SDK: %v", err)
	}
	assertAppFamilyQuotaUsage(t, ctx, repository, accountID, AppKindSDK.String(), "active-sdk", 0, false)
}

// assertAppFamilyQuotaUsage keeps count and target assertions uniform across account and adapter projections.
func assertAppFamilyQuotaUsage(t *testing.T, ctx context.Context, repository Store, accountID uuid.UUID, kind, canonicalName string, wantCurrent int, wantTarget bool) {
	t.Helper()
	got, err := repository.GetAppFamilyQuotaUsage(ctx, accountID, kind, canonicalName)
	// A count failure invalidates the entitlement result and must fail the fixture immediately.
	if err != nil {
		t.Fatalf("GetAppFamilyQuotaUsage(%q, %q): %v", kind, canonicalName, err)
	}
	// Exact counts prove retained and non-runnable rows never leak into the quota projection.
	if got.CurrentInvokable != wantCurrent || got.TargetInvokable != wantTarget {
		t.Fatalf("GetAppFamilyQuotaUsage(%q, %q) = %#v, want current=%d target=%t", kind, canonicalName, got, wantCurrent, wantTarget)
	}
}
