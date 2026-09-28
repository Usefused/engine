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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestExecutionAppBundleDigestIsVersionIdentity prevents a source label from replacing compiled code on reapply.
func TestExecutionAppBundleDigestIsVersionIdentity(t *testing.T) {
	first := App{SourceHash: "sha256:source", BundleDigest: ExecutionAppBundleDigest([]byte("first")), Selections: []byte("[]")}
	// The control case must be immutable-equal so a malformed fixture cannot make the mismatch assertion vacuous.
	if !sameImmutableAppVersion(first, first) {
		t.Fatal("identical compiled code was not recognized as one immutable version")
	}
	second := first
	second.BundleDigest = ExecutionAppBundleDigest([]byte("second"))
	// The same caller-supplied source hash does not make distinct compiled bytes an idempotent apply.
	if sameImmutableAppVersion(first, second) {
		t.Fatal("changed compiler output accepted as the same immutable version")
	}
}

// TestExecutionAppBundleAdmissionBounds protects the database artifact limit and object-shaped manifest contract.
func TestExecutionAppBundleAdmissionBounds(t *testing.T) {
	valid := ExecutionAppBundle{AppID: uuid.New(), SourceHash: "sha256:source", BundleJS: "exports.run = () => 1;", Manifest: json.RawMessage(`{"schemaVersion":1,"inputSchema":{},"outputSchema":{},"searchable":[],"selectedOperations":[]}`)}
	cases := []struct {
		name    string
		bundle  ExecutionAppBundle
		wantErr bool
	}{
		{name: "valid", bundle: valid},
		{name: "no identity", bundle: ExecutionAppBundle{SourceHash: valid.SourceHash, BundleJS: valid.BundleJS, Manifest: valid.Manifest}, wantErr: true},
		{name: "empty source", bundle: ExecutionAppBundle{AppID: valid.AppID, BundleJS: valid.BundleJS, Manifest: valid.Manifest}, wantErr: true},
		{name: "oversized bundle", bundle: ExecutionAppBundle{AppID: valid.AppID, SourceHash: valid.SourceHash, BundleJS: strings.Repeat("x", maxExecutionAppBundleBytes+1), Manifest: valid.Manifest}, wantErr: true},
		{name: "array manifest", bundle: ExecutionAppBundle{AppID: valid.AppID, SourceHash: valid.SourceHash, BundleJS: valid.BundleJS, Manifest: json.RawMessage(`[]`)}, wantErr: true},
	}
	for _, testCase := range cases {
		// Each boundary is checked independently so an earlier malformed field cannot hide a later regression.
		t.Run(testCase.name, func(t *testing.T) {
			err := validateExecutionAppBundle(testCase.bundle)
			// Admission should reject only the explicitly malformed artifact cases.
			if (err != nil) != testCase.wantErr {
				t.Fatalf("validateExecutionAppBundle() error = %v, want error %t", err, testCase.wantErr)
			}
		})
	}
}

// TestExecutionAppBundleImmutableVersions verifies exact-version writes, reads, and cascade cleanup in PostgreSQL.
func TestExecutionAppBundleImmutableVersions(t *testing.T) {
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
		VALUES ($1, $2, 'execution', $3, 'Bundle test', 'typescript', $4)
	`, familyID, accountID, "bundle-"+familyID.String(), teamID)
	// Both app rows must share one family so lookups prove exact version identity.
	if err != nil {
		t.Fatalf("seed app family: %v", err)
	}
	manifest := json.RawMessage(`{"schemaVersion":1,"inputSchema":{},"outputSchema":{},"searchable":[],"selectedOperations":[]}`)
	first := ExecutionAppBundle{AppID: firstID, SourceHash: "sha256:first", BundleJS: "exports.run = () => 1;", Manifest: manifest}
	second := ExecutionAppBundle{AppID: secondID, SourceHash: "sha256:second", BundleJS: "exports.run = () => 2;", Manifest: manifest}
	_, err = pool.Exec(ctx, `
		INSERT INTO fused_apps (app_id, app_family_id, account_id, version, config_key, source_hash, bundle_digest, hosted_mcp, status)
		VALUES ($1, $3, $4, '1.0.0', $5, 'sha256:first', $7, true, 'active'),
		       ($2, $3, $4, '2.0.0', $6, 'sha256:second', $8, true, 'active')
	`, firstID, secondID, familyID, accountID, "bundle:first:"+firstID.String(), "bundle:second:"+secondID.String(), ExecutionAppBundleDigest([]byte(first.BundleJS)), ExecutionAppBundleDigest([]byte(second.BundleJS)))
	// Persistence must see both exact version foreign-key targets before saving artifacts.
	if err != nil {
		t.Fatalf("seed app versions: %v", err)
	}
	repository := NewPostgresStore(pool).(ExecutionAppBundleStore)
	targets := NewPostgresStore(pool).(ExecutionAppTargetStore)
	warm := NewPostgresStore(pool).(ExecutionAppWarmStore)
	routes := NewPostgresStore(pool).(interface {
		ResolveMCPRoute(context.Context, uuid.UUID) (*MCPRouteTarget, error)
	})
	assertExecutionAppBundleRequiresPlannedDigest(t, ctx, pool, repository, first)
	assertExecutionAppBundleCreation(t, ctx, repository, targets, warm, routes, familyID, first, second)
	assertExecutionAppBundleReads(t, ctx, repository, first, second)
	assertExecutionAppBundleRejectsTampering(t, ctx, pool, repository, firstID)
	assertExecutionAppBundleCascade(t, ctx, pool, repository, targets, databaseURL, firstID, secondID)
}

// assertExecutionAppBundleRequiresPlannedDigest proves an app.manage caller cannot supply code absent from the plan.
func assertExecutionAppBundleRequiresPlannedDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository ExecutionAppBundleStore, first ExecutionAppBundle) {
	t.Helper()
	// An app.manage caller cannot attach compiled code before the exact version has a planned digest.
	if _, err := pool.Exec(ctx, `UPDATE fused_apps SET bundle_digest = NULL WHERE app_id = $1`, first.AppID); err != nil {
		t.Fatalf("remove planned digest: %v", err)
	}
	if err := repository.CreateExecutionAppBundle(ctx, first); !errors.Is(err, ErrExecutionAppBundleNotFound) {
		t.Fatalf("unpinned bundle error = %v, want unavailable version", err)
	}
	// A pinned digest for different compiled bytes is equally unable to authorize this artifact.
	if _, err := pool.Exec(ctx, `UPDATE fused_apps SET bundle_digest = $2 WHERE app_id = $1`, first.AppID, ExecutionAppBundleDigest([]byte("different"))); err != nil {
		t.Fatalf("set wrong planned digest: %v", err)
	}
	if err := repository.CreateExecutionAppBundle(ctx, first); !errors.Is(err, ErrExecutionAppBundleNotFound) {
		t.Fatalf("changed-byte bundle error = %v, want unavailable version", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_apps SET bundle_digest = $2 WHERE app_id = $1`, first.AppID, ExecutionAppBundleDigest([]byte(first.BundleJS))); err != nil {
		t.Fatalf("restore planned digest: %v", err)
	}
}

// assertExecutionAppBundleRejectsTampering keeps stored-byte corruption from executing under a valid source label.
func assertExecutionAppBundleRejectsTampering(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository ExecutionAppBundleStore, appID uuid.UUID) {
	t.Helper()
	// Even database corruption cannot turn an attached artifact into executable code with a valid source label.
	if _, err := pool.Exec(ctx, `UPDATE fused_execution_app_bundles SET bundle_js = $2 WHERE app_id = $1`, appID, "exports.run = () => 99;"); err != nil {
		t.Fatalf("mutate stored bundle for integrity test: %v", err)
	}
	if _, err := repository.GetExecutionAppBundle(ctx, appID); !errors.Is(err, ErrExecutionAppBundleDigestMismatch) {
		t.Fatalf("tampered bundle read error = %v, want digest mismatch", err)
	}
}

// assertExecutionAppBundleCreation proves exact siblings, semantic retry, and write-once conflict behavior.
func assertExecutionAppBundleCreation(t *testing.T, ctx context.Context, repository ExecutionAppBundleStore, targets ExecutionAppTargetStore, warm ExecutionAppWarmStore, routes interface {
	ResolveMCPRoute(context.Context, uuid.UUID) (*MCPRouteTarget, error)
}, familyID uuid.UUID, first, second ExecutionAppBundle) {
	t.Helper()
	assertExecutionAppBundlePromotion(t, ctx, repository, targets, first, second)
	assertExecutionAppPromotedRoutes(t, ctx, routes, familyID, first.AppID, second.AppID)
	assertExecutionAppWarmTarget(t, ctx, warm, familyID, second)
	assertExecutionAppBundleRetryAndRollback(t, ctx, repository, targets, first, second)
}

// assertExecutionAppBundlePromotion proves only an attached bundle can become a traffic target.
func assertExecutionAppBundlePromotion(t *testing.T, ctx context.Context, repository ExecutionAppBundleStore, targets ExecutionAppTargetStore, first, second ExecutionAppBundle) {
	t.Helper()
	// Applying a version alone cannot interrupt the previous ready version.
	assertExecutionAppTarget(t, ctx, targets, first.AppID, false)
	assertExecutionAppTarget(t, ctx, targets, second.AppID, false)
	// Distinct app IDs in one family must retain their own compiled source and manifest.
	if err := repository.CreateExecutionAppBundle(ctx, first); err != nil {
		t.Fatalf("create first bundle: %v", err)
	}
	assertExecutionAppTarget(t, ctx, targets, first.AppID, true)
	if err := repository.CreateExecutionAppBundle(ctx, second); err != nil {
		t.Fatalf("create second bundle: %v", err)
	}
	// The new ready version is the only admitted target.
	assertExecutionAppTarget(t, ctx, targets, first.AppID, false)
	assertExecutionAppTarget(t, ctx, targets, second.AppID, true)
}

// assertExecutionAppPromotedRoutes checks both pinned and stable MCP routes after promotion.
func assertExecutionAppPromotedRoutes(t *testing.T, ctx context.Context, routes interface {
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

// assertExecutionAppWarmTarget requires exactly the promoted version in the prewarm query.
func assertExecutionAppWarmTarget(t *testing.T, ctx context.Context, warm ExecutionAppWarmStore, familyID uuid.UUID, second ExecutionAppBundle) {
	t.Helper()
	listed, err := warm.ListWarmExecutionAppBundles(ctx)
	// An invalid target list cannot be treated as an empty warm plan.
	if err != nil {
		t.Fatalf("list warm execution apps: %v", err)
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

// assertExecutionAppBundleRetryAndRollback preserves immutable bytes while changing only the family pointer.
func assertExecutionAppBundleRetryAndRollback(t *testing.T, ctx context.Context, repository ExecutionAppBundleStore, targets ExecutionAppTargetStore, first, second ExecutionAppBundle) {
	t.Helper()
	// JSONB equality makes a semantically identical retry idempotent despite whitespace.
	identical := first
	identical.Manifest = json.RawMessage(`{ "schemaVersion": 1, "inputSchema": {}, "outputSchema": {}, "searchable": [], "selectedOperations": [] }`)
	if err := repository.CreateExecutionAppBundle(ctx, identical); err != nil {
		t.Fatalf("retry identical bundle: %v", err)
	}
	// An explicit exact-version reapply can roll the family back without replacing immutable code.
	assertExecutionAppTarget(t, ctx, targets, first.AppID, true)
	assertExecutionAppTarget(t, ctx, targets, second.AppID, false)
	changed := first
	changed.BundleJS = "exports.run = () => 99;"
	// Replacing any compiled script for the same app version must be rejected.
	if err := repository.CreateExecutionAppBundle(ctx, changed); !errors.Is(err, ErrExecutionAppBundleImmutable) {
		t.Fatalf("changed bundle error = %v, want immutable", err)
	}
}

// assertExecutionAppTarget verifies that promotion changes one family pointer, never both siblings.
func assertExecutionAppTarget(t *testing.T, ctx context.Context, targets ExecutionAppTargetStore, appID uuid.UUID, want bool) {
	t.Helper()
	active, err := targets.IsExecutionAppTrafficTarget(ctx, appID)
	// A failed target query cannot masquerade as a correctly denied version.
	if err != nil || active != want {
		t.Fatalf("execution app target %s = %t, %v; want %t", appID, active, err, want)
	}
}

// assertExecutionAppBundleReads proves an immutable conflict cannot alter either exact version.
func assertExecutionAppBundleReads(t *testing.T, ctx context.Context, repository ExecutionAppBundleStore, first, second ExecutionAppBundle) {
	t.Helper()
	loadedFirst, err := repository.GetExecutionAppBundle(ctx, first.AppID)
	// The rejected replacement cannot alter the retained exact-version bundle.
	if err != nil || loadedFirst.BundleJS != first.BundleJS || loadedFirst.SourceHash != first.SourceHash {
		t.Fatalf("first bundle = (%#v, %v)", loadedFirst, err)
	}
	loadedSecond, err := repository.GetExecutionAppBundle(ctx, second.AppID)
	// Fetch by app ID must not resolve to another version in the same family.
	if err != nil || loadedSecond.BundleJS != second.BundleJS || loadedSecond.AppID != second.AppID {
		t.Fatalf("second bundle = (%#v, %v)", loadedSecond, err)
	}
}

// assertExecutionAppBundleCascade proves version deletion removes only its own compiled artifact.
func assertExecutionAppBundleCascade(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository ExecutionAppBundleStore, targets ExecutionAppTargetStore, databaseURL string, firstID, secondID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx, `DELETE FROM fused_apps WHERE app_id = $1`, firstID)
	// Version deletion must cascade only its own compiled artifact.
	if err != nil {
		t.Fatalf("delete first app version: %v", err)
	}
	if _, err := repository.GetExecutionAppBundle(ctx, firstID); !errors.Is(err, ErrExecutionAppBundleNotFound) {
		t.Fatalf("deleted version lookup error = %v, want not found", err)
	}
	if _, err := repository.GetExecutionAppBundle(ctx, secondID); err != nil {
		t.Fatalf("sibling version bundle was removed: %v", err)
	}
	// An intentionally removed target must not revive the remaining ready sibling during Engine restart convergence.
	restarted, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		t.Fatalf("restart Engine schema after target deletion: %v", err)
	}
	restarted.Close()
	assertExecutionAppTarget(t, ctx, targets, secondID, false)
}
