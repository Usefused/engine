package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestUnifiedAppBundleDigestIsVersionIdentity prevents a source label from replacing compiled code on reapply.
func TestUnifiedAppBundleDigestIsVersionIdentity(t *testing.T) {
	first := App{SourceHash: "sha256:source", BundleDigest: UnifiedAppBundleDigest([]byte("first")), Selections: []byte("[]")}
	// The control case must be immutable-equal so a malformed fixture cannot make the mismatch assertion vacuous.
	if !sameImmutableAppVersion(first, first) {
		t.Fatal("identical compiled code was not recognized as one immutable version")
	}
	second := first
	second.BundleDigest = UnifiedAppBundleDigest([]byte("second"))
	// The same caller-supplied source hash does not make distinct compiled bytes an idempotent apply.
	if sameImmutableAppVersion(first, second) {
		t.Fatal("changed compiler output accepted as the same immutable version")
	}
}

// TestUnifiedAppBundleAdmissionBounds protects the database artifact limit and object-shaped manifest contract.
func TestUnifiedAppBundleAdmissionBounds(t *testing.T) {
	valid := UnifiedAppBundle{AppID: uuid.New(), SourceHash: "sha256:source", BundleJS: "exports.run = () => 1;", Manifest: json.RawMessage(`{"schemaVersion":1,"inputSchema":{},"outputSchema":{},"searchable":[],"selectedOperations":[]}`)}
	cases := []struct {
		name    string
		bundle  UnifiedAppBundle
		wantErr bool
	}{
		{name: "valid", bundle: valid},
		{name: "no identity", bundle: UnifiedAppBundle{SourceHash: valid.SourceHash, BundleJS: valid.BundleJS, Manifest: valid.Manifest}, wantErr: true},
		{name: "empty source", bundle: UnifiedAppBundle{AppID: valid.AppID, BundleJS: valid.BundleJS, Manifest: valid.Manifest}, wantErr: true},
		{name: "oversized bundle", bundle: UnifiedAppBundle{AppID: valid.AppID, SourceHash: valid.SourceHash, BundleJS: strings.Repeat("x", maxUnifiedAppBundleBytes+1), Manifest: valid.Manifest}, wantErr: true},
		{name: "array manifest", bundle: UnifiedAppBundle{AppID: valid.AppID, SourceHash: valid.SourceHash, BundleJS: valid.BundleJS, Manifest: json.RawMessage(`[]`)}, wantErr: true},
	}
	for _, testCase := range cases {
		// Each boundary is checked independently so an earlier malformed field cannot hide a later regression.
		t.Run(testCase.name, func(t *testing.T) {
			err := validateUnifiedAppBundle(testCase.bundle)
			// Admission should reject only the explicitly malformed artifact cases.
			if (err != nil) != testCase.wantErr {
				t.Fatalf("validateUnifiedAppBundle() error = %v, want error %t", err, testCase.wantErr)
			}
		})
	}
}

// TestUnifiedAppBundleImmutableVersions verifies exact-version writes, reads, and cascade cleanup in PostgreSQL.
func TestUnifiedAppBundleImmutableVersions(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Durable bundle behavior needs an explicitly configured local PostgreSQL integration database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Schema initialization must include the bundle table before exact app versions are seeded.
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(pool.Close)
	accountID, familyID, firstID, secondID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	teamID := seedAppOwnerTeam(t, ctx, pool)
	// Cleanup removes only this test's identities, preserving unrelated developer data.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_id IN ($1, $2)`, firstID, secondID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_teams WHERE id = $1`, teamID)
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO fused_app_families (app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'unified_app', $3, 'Bundle test', 'typescript', $4)
	`, familyID, accountID, "bundle-"+familyID.String(), teamID)
	// Both app rows must share one family so lookups prove exact version identity.
	if err != nil {
		t.Fatalf("seed app family: %v", err)
	}
	manifest := json.RawMessage(`{"schemaVersion":1,"inputSchema":{},"outputSchema":{},"searchable":[],"selectedOperations":[]}`)
	first := UnifiedAppBundle{AppID: firstID, SourceHash: "sha256:first", BundleJS: "exports.run = () => 1;", Manifest: manifest}
	second := UnifiedAppBundle{AppID: secondID, SourceHash: "sha256:second", BundleJS: "exports.run = () => 2;", Manifest: manifest}
	_, err = pool.Exec(ctx, `
		INSERT INTO fused_apps (app_id, app_family_id, account_id, version, config_key, source_hash, bundle_digest, hosted_mcp, status, scope_schema_version, selections)
		VALUES ($1, $3, $4, '1.0.0', $5, 'sha256:first', $7, true, 'active', $9, '[]'),
		       ($2, $3, $4, '2.0.0', $6, 'sha256:second', $8, true, 'active', $9, '[]')
	`, firstID, secondID, familyID, accountID, "bundle:first:"+firstID.String(), "bundle:second:"+secondID.String(), UnifiedAppBundleDigest([]byte(first.BundleJS)), UnifiedAppBundleDigest([]byte(second.BundleJS)), models.AppScopeSchemaVersion)
	// Persistence must see both exact version foreign-key targets before saving artifacts.
	if err != nil {
		t.Fatalf("seed app versions: %v", err)
	}
	repository := NewPostgresStore(pool).(UnifiedAppBundleStore)
	targets := NewPostgresStore(pool).(UnifiedAppTargetStore)
	warm := NewPostgresStore(pool).(UnifiedAppWarmStore)
	routes := NewPostgresStore(pool).(interface {
		ResolveMCPRoute(context.Context, uuid.UUID) (*MCPRouteTarget, error)
	})
	assertUnifiedAppBundleRequiresPlannedDigest(t, ctx, pool, repository, first)
	assertUnifiedAppBundleCreation(t, ctx, repository, targets, warm, routes, familyID, first, second)
	assertUnifiedAppTrafficPromotion(t, ctx, pool, familyID, firstID, secondID)
	assertUnifiedAppAttachmentResolution(t, ctx, pool, accountID, familyID)
	assertUnifiedAppBundleReads(t, ctx, repository, first, second)
	assertUnifiedAppBundleRejectsTampering(t, ctx, pool, repository, firstID)
	assertUnifiedAppBundleCascade(t, ctx, pool, repository, targets, databaseURL, firstID, secondID)
}

// assertUnifiedAppBundleRequiresPlannedDigest proves an app.manage caller cannot supply code absent from the plan.
func assertUnifiedAppBundleRequiresPlannedDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository UnifiedAppBundleStore, first UnifiedAppBundle) {
	t.Helper()
	// An app.manage caller cannot attach compiled code before the exact version has a planned digest.
	if _, err := pool.Exec(ctx, `UPDATE fused_apps SET bundle_digest = NULL WHERE app_id = $1`, first.AppID); err != nil {
		t.Fatalf("remove planned digest: %v", err)
	}
	if err := repository.CreateUnifiedAppBundle(ctx, first); !errors.Is(err, ErrUnifiedAppBundleNotFound) {
		t.Fatalf("unpinned bundle error = %v, want unavailable version", err)
	}
	// A pinned digest for different compiled bytes is equally unable to authorize this artifact.
	if _, err := pool.Exec(ctx, `UPDATE fused_apps SET bundle_digest = $2 WHERE app_id = $1`, first.AppID, UnifiedAppBundleDigest([]byte("different"))); err != nil {
		t.Fatalf("set wrong planned digest: %v", err)
	}
	if err := repository.CreateUnifiedAppBundle(ctx, first); !errors.Is(err, ErrUnifiedAppBundleNotFound) {
		t.Fatalf("changed-byte bundle error = %v, want unavailable version", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_apps SET bundle_digest = $2 WHERE app_id = $1`, first.AppID, UnifiedAppBundleDigest([]byte(first.BundleJS))); err != nil {
		t.Fatalf("restore planned digest: %v", err)
	}
}

// assertUnifiedAppBundleRejectsTampering keeps stored-byte corruption from executing under a valid source label.
func assertUnifiedAppBundleRejectsTampering(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository UnifiedAppBundleStore, appID uuid.UUID) {
	t.Helper()
	// Even database corruption cannot turn an attached artifact into executable code with a valid source label.
	if _, err := pool.Exec(ctx, `UPDATE fused_unified_app_bundles SET bundle_js = $2 WHERE app_id = $1`, appID, "exports.run = () => 99;"); err != nil {
		t.Fatalf("mutate stored bundle for integrity test: %v", err)
	}
	if _, err := repository.GetUnifiedAppBundle(ctx, appID); !errors.Is(err, ErrUnifiedAppBundleDigestMismatch) {
		t.Fatalf("tampered bundle read error = %v, want digest mismatch", err)
	}
}

// assertUnifiedAppBundleCreation proves exact siblings, semantic retry, and write-once conflict behavior.
func assertUnifiedAppBundleCreation(t *testing.T, ctx context.Context, repository UnifiedAppBundleStore, targets UnifiedAppTargetStore, warm UnifiedAppWarmStore, routes interface {
	ResolveMCPRoute(context.Context, uuid.UUID) (*MCPRouteTarget, error)
}, familyID uuid.UUID, first, second UnifiedAppBundle) {
	t.Helper()
	assertUnifiedAppBundlePromotion(t, ctx, repository, targets, first, second)
	assertUnifiedAppPromotedRoutes(t, ctx, routes, familyID, first.AppID, second.AppID)
	assertUnifiedAppWarmTarget(t, ctx, warm, familyID, second)
	assertUnifiedAppBundleRetryAndRollback(t, ctx, repository, targets, first, second)
}

// assertUnifiedAppBundlePromotion proves only an attached bundle can become a traffic target.
func assertUnifiedAppBundlePromotion(t *testing.T, ctx context.Context, repository UnifiedAppBundleStore, targets UnifiedAppTargetStore, first, second UnifiedAppBundle) {
	t.Helper()
	// Applying a version alone cannot interrupt the previous ready version.
	assertUnifiedAppTarget(t, ctx, targets, first.AppID, false)
	assertUnifiedAppTarget(t, ctx, targets, second.AppID, false)
	// Distinct app IDs in one family must retain their own compiled source and manifest.
	if err := repository.CreateUnifiedAppBundle(ctx, first); err != nil {
		t.Fatalf("create first bundle: %v", err)
	}
	assertUnifiedAppTarget(t, ctx, targets, first.AppID, true)
	if err := repository.CreateUnifiedAppBundle(ctx, second); err != nil {
		t.Fatalf("create second bundle: %v", err)
	}
	// The new ready version is the only admitted target.
	assertUnifiedAppTarget(t, ctx, targets, first.AppID, false)
	assertUnifiedAppTarget(t, ctx, targets, second.AppID, true)
}

// assertUnifiedAppPromotedRoutes checks both pinned and stable MCP routes after promotion.
func assertUnifiedAppPromotedRoutes(t *testing.T, ctx context.Context, routes interface {
	ResolveMCPRoute(context.Context, uuid.UUID) (*MCPRouteTarget, error)
}, familyID, firstID, secondID uuid.UUID) {
	t.Helper()
	// A pinned MCP route cannot bypass the new family target after promotion.
	if _, err := routes.ResolveMCPRoute(ctx, firstID); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("old pinned MCP route error = %v, want not found", err)
	}
	if route, err := routes.ResolveMCPRoute(ctx, secondID); err != nil || route == nil || route.AppID != secondID {
		t.Fatalf("current pinned MCP route = %#v, %v", route, err)
	}
	// The stable family route must move only with the ready bundle, not with an earlier apply.
	if route, err := routes.ResolveMCPRoute(ctx, familyID); err != nil || route == nil || route.AppID != secondID || !route.Stable {
		t.Fatalf("current stable MCP route = %#v, %v", route, err)
	}
}

// assertUnifiedAppWarmTarget requires exactly the promoted version in the prewarm query.
func assertUnifiedAppWarmTarget(t *testing.T, ctx context.Context, warm UnifiedAppWarmStore, familyID uuid.UUID, second UnifiedAppBundle) {
	t.Helper()
	listed, err := warm.ListWarmUnifiedAppBundles(ctx)
	// An invalid target list cannot be treated as an empty warm plan.
	if err != nil {
		t.Fatalf("list warm unified apps: %v", err)
	}
	var fixtureTargets int
	for _, candidate := range listed {
		// Other fixture families may share an integration database, but this family gets exactly one warm worker.
		if candidate.FamilyID == familyID {
			fixtureTargets++
			if candidate.AppID != second.AppID || candidate.BundleJS != second.BundleJS {
				t.Fatalf("warm target for fixture family = %#v", candidate)
			}
		}
	}
	if fixtureTargets != 1 {
		t.Fatalf("warm target count for fixture family = %d, want 1", fixtureTargets)
	}
}

// assertUnifiedAppBundleRetryAndRollback preserves immutable bytes while changing only the family pointer.
func assertUnifiedAppBundleRetryAndRollback(t *testing.T, ctx context.Context, repository UnifiedAppBundleStore, targets UnifiedAppTargetStore, first, second UnifiedAppBundle) {
	t.Helper()
	// JSONB equality makes a semantically identical retry idempotent despite whitespace.
	identical := first
	identical.Manifest = json.RawMessage(`{ "schemaVersion": 1, "inputSchema": {}, "outputSchema": {}, "searchable": [], "selectedOperations": [] }`)
	if err := repository.CreateUnifiedAppBundle(ctx, identical); err != nil {
		t.Fatalf("retry identical bundle: %v", err)
	}
	// An explicit exact-version reapply can roll the family back without replacing immutable code.
	assertUnifiedAppTarget(t, ctx, targets, first.AppID, true)
	assertUnifiedAppTarget(t, ctx, targets, second.AppID, false)
	changed := first
	changed.BundleJS = "exports.run = () => 99;"
	// Replacing any compiled script for the same app version must be rejected.
	if err := repository.CreateUnifiedAppBundle(ctx, changed); !errors.Is(err, ErrUnifiedAppBundleImmutable) {
		t.Fatalf("changed bundle error = %v, want immutable", err)
	}
}

// assertUnifiedAppTarget verifies that promotion changes one family pointer, never both siblings.
func assertUnifiedAppTarget(t *testing.T, ctx context.Context, targets UnifiedAppTargetStore, appID uuid.UUID, want bool) {
	t.Helper()
	active, err := targets.IsUnifiedAppTrafficTarget(ctx, appID)
	// A failed target query cannot masquerade as a correctly denied version.
	if err != nil || active != want {
		t.Fatalf("unified app target %s = %t, %v; want %t", appID, active, err, want)
	}
}

// assertUnifiedAppBundleReads proves an immutable conflict cannot alter either exact version.
func assertUnifiedAppBundleReads(t *testing.T, ctx context.Context, repository UnifiedAppBundleStore, first, second UnifiedAppBundle) {
	t.Helper()
	loadedFirst, err := repository.GetUnifiedAppBundle(ctx, first.AppID)
	// The rejected replacement cannot alter the retained exact-version bundle.
	if err != nil || loadedFirst.BundleJS != first.BundleJS || loadedFirst.SourceHash != first.SourceHash {
		t.Fatalf("first bundle = (%#v, %v)", loadedFirst, err)
	}
	loadedSecond, err := repository.GetUnifiedAppBundle(ctx, second.AppID)
	// Fetch by app ID must not resolve to another version in the same family.
	if err != nil || loadedSecond.BundleJS != second.BundleJS || loadedSecond.AppID != second.AppID {
		t.Fatalf("second bundle = (%#v, %v)", loadedSecond, err)
	}
}

// assertUnifiedAppBundleCascade proves version deletion removes only its own compiled artifact.
func assertUnifiedAppBundleCascade(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository UnifiedAppBundleStore, targets UnifiedAppTargetStore, databaseURL string, firstID, secondID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx, `DELETE FROM fused_apps WHERE app_id = $1`, firstID)
	// Version deletion must cascade only its own compiled artifact.
	if err != nil {
		t.Fatalf("delete first app version: %v", err)
	}
	if _, err := repository.GetUnifiedAppBundle(ctx, firstID); !errors.Is(err, ErrUnifiedAppBundleNotFound) {
		t.Fatalf("deleted version lookup error = %v, want not found", err)
	}
	if _, err := repository.GetUnifiedAppBundle(ctx, secondID); err != nil {
		t.Fatalf("sibling version bundle was removed: %v", err)
	}
	// An intentionally removed target must not revive the remaining ready sibling during Engine restart convergence.
	restarted, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		t.Fatalf("restart Engine schema after target deletion: %v", err)
	}
	restarted.Close()
	assertUnifiedAppTarget(t, ctx, targets, secondID, false)
}
