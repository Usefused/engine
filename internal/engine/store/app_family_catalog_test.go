package store

import (
	"context"
	"strconv"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestAppFamilyCataloguePostgres verifies grouping, authorization, and version-view continuity on the real database.
func TestAppFamilyCataloguePostgres(t *testing.T) {
	for _, kind := range []string{"sdk", "mcp"} {
		// Both adapters must use identical family counting and pagination rules.
		t.Run(kind, func(t *testing.T) { testAppFamilyCataloguePostgres(t, kind) })
	}
}

// testAppFamilyCataloguePostgres seeds one multi-version family alongside empty, other-kind, and foreign-account families.
func testAppFamilyCataloguePostgres(t *testing.T, kind string) {
	fixture := newAppTokenPolicyFixture(t)
	var accountID, ownerID uuid.UUID
	// Read fixture-owned identities without relying on any external database accounts.
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT account_id, owner_team_id FROM fused_app_families WHERE app_family_id=$1`, fixture.familyID).Scan(&accountID, &ownerID); err != nil {
		t.Fatal(err)
	}
	// Family language belongs to the logical SDK and remains stable across all its versions.
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE fused_app_families SET kind=$2, display_name='Alpha:Tools', canonical_name='alpha:tools', target_language=$3 WHERE app_family_id=$1`, fixture.familyID, kind, map[string]any{"sdk": "typescript", "mcp": nil}[kind]); err != nil {
		t.Fatal(err)
	}
	// Multiple pages of immutable versions must still occupy exactly one application row.
	var latestAppID uuid.UUID
	for i := 2; i <= 102; i++ {
		appID := uuid.New()
		// Retain the final inserted identity so the deterministic latest projection can be asserted directly.
		if i == 102 {
			latestAppID = appID
		}
		// Every version has a unique canonical key while sharing its logical family.
		if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status,created_at) VALUES ($1,$2,$3,$4,$5,'catalogue-test','active',NOW() + ($6 * INTERVAL '1 second'))`, appID, fixture.familyID, accountID, strconv.Itoa(i), "catalogue:"+uuid.NewString(), i); err != nil {
			t.Fatal(err)
		}
	}
	// Only an explicit MCP promotion establishes the stable endpoint; SDKs have no implicit version.
	if kind == "mcp" {
		// Promote the oldest version to ensure discovery never chooses the newest sibling.
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE fused_app_families SET mcp_stable_app_id=$2, mcp_stable_route_initialized=true WHERE app_family_id=$1`, fixture.familyID, fixture.appID); err != nil {
			t.Fatal(err)
		}
	}
	otherKind := map[string]string{"sdk": "mcp", "mcp": "sdk"}[kind]
	for _, extra := range []struct {
		account    uuid.UUID
		kind, name string
	}{{accountID, kind, "beta"}, {accountID, otherKind, "other-kind"}, {uuid.New(), kind, "foreign"}} {
		id := uuid.New()
		// Extra families exercise kind/account filtering and retention of logical identity without live versions.
		if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,owner_team_id,target_language) VALUES ($1,$2,$3,$4,$4,$5,$6)`, id, extra.account, extra.kind, extra.name, ownerID, map[string]any{"sdk": "typescript", "mcp": nil}[extra.kind]); err != nil {
			t.Fatal(err)
		}
		// Remove only this fixture's family rows after the assertions complete.
		t.Cleanup(func() {
			_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, id)
		})
	}
	all := accesscontrol.AuthorizedScope{All: true}
	items, total, err := fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, all, kind, "", 1, 0)
	// Version counts must be aggregated before LIMIT, with unrelated accounts/kinds excluded from totals.
	if err != nil || total != 2 || len(items) != 1 || items[0].Name != "Alpha:Tools" || items[0].VersionCount != 102 {
		t.Fatalf("first page: %#v, total %d, error %v", items, total, err)
	}
	// Latest metadata is the newest immutable publication and supplies only catalogue navigation identity.
	if items[0].LatestAppID != latestAppID || items[0].LatestVersion != "102" || items[0].LatestStatus != AppStatusActive || items[0].LatestCreatedAt == nil {
		t.Fatalf("latest version: %#v", items[0])
	}
	// Promotion metadata is an explicit MCP pointer, never an arbitrary SDK version.
	if kind == "mcp" && (items[0].StableAppID != fixture.appID || items[0].StableVersion != "1.0.0") {
		t.Fatalf("promotion: %#v", items[0])
	}
	items, total, err = fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, all, kind, "", 1, 1)
	// Family pagination must include retained identities with zero live versions.
	if err != nil || total != 2 || len(items) != 1 || items[0].Name != "beta" || items[0].VersionCount != 0 {
		t.Fatalf("second page: %#v, total %d, error %v", items, total, err)
	}
	scoped := accesscontrol.AuthorizedScope{IDs: []uuid.UUID{fixture.familyID}}
	items, total, err = fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, scoped, kind, "", 20, 0)
	// ACL filtering must occur before counts and pagination, not after rows have been exposed.
	if err != nil || total != 1 || len(items) != 1 || items[0].AppFamilyID != fixture.familyID {
		t.Fatalf("authorized page: %#v, total %d, error %v", items, total, err)
	}
	items, total, err = fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, accesscontrol.AuthorizedScope{}, kind, "", 20, 0)
	// An actor without app.read grants receives neither rows nor hidden application counts.
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("denied scope: %#v, total %d, error %v", items, total, err)
	}
	items, total, err = fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, all, kind, "Alpha:", 20, 0)
	// Search matches canonical display names without breaking punctuation or counting versions as apps.
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("search: %#v, total %d, error %v", items, total, err)
	}
	versions, total, err := fixture.repository.ListAuthorizedAppsByAccount(fixture.ctx, accountID, all, kind, "", "", 100, 0)
	// Dedicated version discovery still receives exact immutable rows for detail-page switching.
	if err != nil || total != 102 || len(versions) != 100 {
		t.Fatalf("version list: count %d, total %d, error %v", len(versions), total, err)
	}
	siblings, err := fixture.repository.ListAuthorizedAppsByFamily(fixture.ctx, accountID, fixture.familyID, all)
	// Detail-page version selectors continue to retrieve every sibling through the exact-version contract.
	if err != nil || len(siblings) != 102 {
		t.Fatalf("version selector: count %d, error %v", len(siblings), err)
	}
}
