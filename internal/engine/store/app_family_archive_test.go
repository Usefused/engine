package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestArchiveAppFamilyReleasesNameAndRetainsCatalogueHistory verifies the delete/archive identity split on PostgreSQL.
func TestArchiveAppFamilyReleasesNameAndRetainsCatalogueHistory(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	var accountID, ownerTeamID uuid.UUID
	// The fixture supplies valid workspace and ownership identities for every archived dependency.
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT account_id, owner_team_id FROM fused_app_families WHERE app_family_id = $1`, fixture.familyID).Scan(&accountID, &ownerTeamID); err != nil {
		t.Fatal(err)
	}
	name := "reusable-" + uuid.NewString()
	original, created, err := fixture.repository.CreateOrGetAppFamily(fixture.ctx, AppFamily{
		AppFamilyID: uuid.New(), AccountID: accountID, Kind: AppKindSDK,
		CanonicalName: name, DisplayName: name, TargetLanguage: "typescript", OwnerTeamID: ownerTeamID,
	})
	// The test requires a fresh family so name reuse cannot pass through idempotency.
	if err != nil || !created {
		t.Fatalf("create original family: created=%t family=%#v err=%v", created, original, err)
	}
	appID := uuid.New()
	// A real deactivation supplies the tombstone that family deletion must preserve.
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_apps (app_id, app_family_id, account_id, version, config_key, source_hash, status)
		VALUES ($1, $2, $3, '1.0.0', $4, 'archive-test', 'active')
		`, appID, original.AppFamilyID, accountID, "sdk:archive:"+uuid.NewString()); err != nil {
		t.Fatalf("create original app version: %v", err)
	}
	// Exact-version cleanup must complete before the family archive can proceed.
	if err := fixture.repository.DeactivateAppVersion(fixture.ctx, appID, uuid.Nil); err != nil {
		t.Fatalf("deactivate original app version: %v", err)
	}
	bucket, err := fixture.repository.CreateBucket(fixture.ctx, "archive-"+uuid.NewString(), false)
	// The bucket is isolated so cleanup assertions cannot disturb existing workspace data.
	if err != nil {
		t.Fatalf("create family bucket: %v", err)
	}
	// Cleanup targets only random identities created by this test.
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_family_id IN (SELECT app_family_id FROM fused_app_families WHERE canonical_name = $1)`, name)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_app_tombstones WHERE app_family_id IN (SELECT app_family_id FROM fused_app_families WHERE canonical_name = $1)`, name)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE canonical_name = $1`, name)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_buckets WHERE id = $1`, bucket.ID)
	})
	// A persisted family binding proves archive cleanup releases the bucket dependency.
	if err := fixture.repository.SetAppFamilyBucket(fixture.ctx, original.AppFamilyID, bucket.ID); err != nil {
		t.Fatalf("bind family bucket: %v", err)
	}
	tokenID := uuid.New()
	// A live token exercises both executable-hash removal and history retention.
	if _, err := fixture.repository.CreateAppToken(fixture.ctx, AppTokenIssue{
		ID: tokenID, AppFamilyID: original.AppFamilyID, TokenHash: "archive-" + uuid.NewString(), Name: "archive-token",
		Policy: AppTokenPolicy{AllowAll: true}, BindingMode: AppTokenBindingDynamic,
	}); err != nil {
		t.Fatalf("create family token: %v", err)
	}
	// A resource-scoped manager grant must be removed rather than inherited by the replacement family.
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id)
		SELECT 'team', $1, role.id, 'app', $2 FROM fused_roles role WHERE role.slug = $3
		`, ownerTeamID, original.AppFamilyID, accesscontrol.RoleAppManager); err != nil {
		t.Fatalf("create family grant: %v", err)
	}
	// The mutation should atomically clean dependencies and retain historical identity.
	if err := fixture.repository.ArchiveAppFamily(fixture.ctx, accountID, original.AppFamilyID); err != nil {
		t.Fatalf("archive family: %v", err)
	}
	var activeTokens, bucketBindings, roleBindings, tombstones int
	var tokenStatus AppTokenStatus
	// One query checks the complete post-archive persistence boundary.
	if err := fixture.pool.QueryRow(fixture.ctx, `
		SELECT
			(SELECT COUNT(*) FROM fused_app_tokens WHERE app_family_id = $1),
			(SELECT COUNT(*) FROM fused_app_family_buckets WHERE app_family_id = $1),
			(SELECT COUNT(*) FROM fused_role_bindings WHERE resource_type = 'app' AND resource_id = $1),
			(SELECT COUNT(*) FROM fused_app_tombstones WHERE app_family_id = $1),
			(SELECT status FROM fused_app_token_history WHERE id = $2)
	`, original.AppFamilyID, tokenID).Scan(&activeTokens, &bucketBindings, &roleBindings, &tombstones, &tokenStatus); err != nil {
		t.Fatalf("inspect archived family state: %v", err)
	}
	// Deletion removes every executable or authorization binding while retaining revoked token evidence.
	if activeTokens != 0 || bucketBindings != 0 || roleBindings != 0 || tombstones != 1 || tokenStatus != AppTokenStatusRevoked {
		t.Fatalf("archive cleanup active_tokens=%d bucket_bindings=%d role_bindings=%d tombstones=%d token_status=%s", activeTokens, bucketBindings, roleBindings, tombstones, tokenStatus)
	}
	// Operational lookups must not revive a retained historical family.
	if _, err := fixture.repository.GetAppFamily(fixture.ctx, original.AppFamilyID); !errors.Is(err, ErrAppFamilyNotFound) {
		t.Fatalf("archived family lookup error = %v", err)
	}
	replacement, created, err := fixture.repository.CreateOrGetAppFamily(fixture.ctx, AppFamily{
		AppFamilyID: uuid.New(), AccountID: accountID, Kind: AppKindSDK,
		CanonicalName: name, DisplayName: name, TargetLanguage: "typescript", OwnerTeamID: ownerTeamID,
	})
	// Reuse must allocate a new authorization boundary rather than reviving the archived UUID.
	if err != nil || !created || replacement.AppFamilyID == original.AppFamilyID {
		t.Fatalf("reuse family name: created=%t family=%#v err=%v", created, replacement, err)
	}
	all := accesscontrol.AuthorizedScope{All: true}
	archived, total, err := fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, all, "sdk", name, true, 20, 0)
	// Archive discovery must return only the retained old identity with its deletion timestamp.
	if err != nil || total != 1 || len(archived) != 1 || archived[0].AppFamilyID != original.AppFamilyID || archived[0].ArchivedAt == nil {
		t.Fatalf("archive catalogue: items=%#v total=%d err=%v", archived, total, err)
	}
	live, total, err := fixture.repository.ListAuthorizedAppFamilies(fixture.ctx, accountID, all, "sdk", name, false, 20, 0)
	// Live discovery must resolve the replacement name without exposing the archived collision.
	if err != nil || total != 1 || len(live) != 1 || live[0].AppFamilyID != replacement.AppFamilyID {
		t.Fatalf("live catalogue: items=%#v total=%d err=%v", live, total, err)
	}
}

// TestArchiveAppFamilyRejectsLiveVersions ensures the final logical delete cannot bypass exact-version cleanup.
func TestArchiveAppFamilyRejectsLiveVersions(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	var accountID uuid.UUID
	// The fixture family has one live version, making it an intentional conflict target.
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT account_id FROM fused_app_families WHERE app_family_id = $1`, fixture.familyID).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	// Family deletion must direct callers back through exact-version deactivation.
	if err := fixture.repository.ArchiveAppFamily(fixture.ctx, accountID, fixture.familyID); !errors.Is(err, ErrAppFamilyNotEmpty) {
		t.Fatalf("archive populated family error = %v", err)
	}
}
