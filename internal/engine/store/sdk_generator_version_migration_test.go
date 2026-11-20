package store

import (
	"context"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDeterministicSDKGenerationJobIDMatchesRegistryFormula proves Engine computes the exact SHA-1 job identity Registry emits.
func TestDeterministicSDKGenerationJobIDMatchesRegistryFormula(t *testing.T) {
	accountID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	engineID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	appID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	sourceHash := strings.Repeat("a", 64)

	got := deterministicSDKGenerationJobID(accountID, engineID, appID, sourceHash, models.SDKGeneratorVersion)
	want := uuid.NewSHA1(uuid.NameSpaceOID, []byte("sdk-job:"+strings.Join([]string{
		accountID.String(), engineID.String(), appID.String(), sourceHash, models.SDKGeneratorVersion,
	}, ":"))).String()
	// Exact identity parity ensures pending v5-v7 builds match Registry's v8 replay response on the next worker pass.
	if got != want {
		t.Fatalf("deterministicSDKGenerationJobID() = %q, want %q", got, want)
	}
}

// TestMigrateSDKGeneratorVersionsRetiresPreV5AndUpgradesV5ThroughV7 verifies pre-v5 SDK deletion with tombstones and v5-v7 in-place upgrades.
func TestMigrateSDKGeneratorVersionsRetiresPreV5AndUpgradesV5ThroughV7(t *testing.T) {
	ctx, pool := sourceMigrationTestDatabase(t)
	accountID := uuid.New()
	engineID := uuid.New()
	ownerID := sourceMigrationTestOwner(t, ctx, pool)

	sdkFamilyID := insertSDKGeneratorMigrationFamily(t, ctx, pool, accountID, ownerID, "sdk", "sdk", "migrated-sdk-"+uuid.NewString()[:8])
	apiFamilyID := insertSDKGeneratorMigrationFamily(t, ctx, pool, accountID, ownerID, "sdk", "api", "migrated-api-"+uuid.NewString()[:8])
	mcpFamilyID := insertSDKGeneratorMigrationFamily(t, ctx, pool, accountID, ownerID, "mcp", "", "untouched-mcp-"+uuid.NewString()[:8])

	v1BuildingAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "0.1.0", "registry-generator-v1", "building", "pending", "old-job-v1")
	v4ActiveAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "0.4.0", "registry-generator-v4", "active", "complete", "old-job-v4")
	v5BuildingAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "0.5.0", "registry-generator-v5", "building", "pending", "old-job-v5")
	v6BuildingAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "1.0.0", "registry-generator-v6", "building", "pending", "old-job-v6")
	v7ActiveAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "2.0.0", "registry-generator-v7", "active", "complete", "old-job-v7")
	v7DeprecatedAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "3.0.0", "registry-generator-v7", "deprecated", "complete", "old-job-v7-dep")
	v8ActiveAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "4.0.0", models.SDKGeneratorVersion, "active", "complete", "existing-v8-job")
	v4DirectAPIAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, apiFamilyID, accountID, ownerID, "0.4.0", "registry-generator-v4", "active", "skipped", "")
	v7DirectAPIAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, apiFamilyID, accountID, ownerID, "1.0.0", "registry-generator-v7", "active", "skipped", "")
	mcpAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, mcpFamilyID, accountID, ownerID, "1.0.0", "", "active", "", "")

	migrator := NewPostgresStore(pool).(SDKGeneratorVersionMigrationStore)
	// Missing Engine ID must fail closed before mutating job-backed v5-v7 SDK builds (while retired pre-v5 rows are already deleted).
	if _, err := migrator.MigrateSDKGeneratorVersions(ctx, uuid.Nil); err == nil {
		t.Fatal("expected error when Engine ID is nil for job-backed v5-v7 SDK rows")
	}
	assertSDKGeneratorRowDeletedAndTombstoned(t, ctx, pool, v1BuildingAppID, sdkFamilyID, "0.1.0")
	assertSDKGeneratorRowDeletedAndTombstoned(t, ctx, pool, v4ActiveAppID, sdkFamilyID, "0.4.0")

	// Insert another pre-v5 row to verify DeletedRows and MigratedRows together on a clean full pass.
	v3DeprecatedAppID := insertSDKGeneratorMigrationApp(t, ctx, pool, sdkFamilyID, accountID, ownerID, "0.3.0", "registry-generator-v3", "deprecated", "complete", "old-job-v3")

	first, err := migrator.MigrateSDKGeneratorVersions(ctx, engineID)
	// One remaining pre-v5 SDK is deleted and five v5-v7 rows (four job-backed plus one jobless Direct API) are upgraded.
	if err != nil || first.DeletedRows != 1 || first.MigratedRows != 5 || first.ReboundJobIDs != 4 {
		t.Fatalf("first migration = %+v, err = %v", first, err)
	}

	assertSDKGeneratorRowDeletedAndTombstoned(t, ctx, pool, v3DeprecatedAppID, sdkFamilyID, "0.3.0")
	assertSDKGeneratorRowState(t, ctx, pool, v5BuildingAppID, models.SDKGeneratorVersion, deterministicSDKGenerationJobID(accountID, engineID, v5BuildingAppID, strings.Repeat("a", 64), models.SDKGeneratorVersion))
	assertSDKGeneratorRowState(t, ctx, pool, v6BuildingAppID, models.SDKGeneratorVersion, deterministicSDKGenerationJobID(accountID, engineID, v6BuildingAppID, strings.Repeat("a", 64), models.SDKGeneratorVersion))
	assertSDKGeneratorRowState(t, ctx, pool, v7ActiveAppID, models.SDKGeneratorVersion, deterministicSDKGenerationJobID(accountID, engineID, v7ActiveAppID, strings.Repeat("a", 64), models.SDKGeneratorVersion))
	assertSDKGeneratorRowState(t, ctx, pool, v7DeprecatedAppID, models.SDKGeneratorVersion, deterministicSDKGenerationJobID(accountID, engineID, v7DeprecatedAppID, strings.Repeat("a", 64), models.SDKGeneratorVersion))
	assertSDKGeneratorRowState(t, ctx, pool, v7DirectAPIAppID, models.SDKGeneratorVersion, "")
	// Direct APIs never generate Registry packages and must never be auto-deleted by the pre-v5 retirement pass.
	assertSDKGeneratorRowState(t, ctx, pool, v4DirectAPIAppID, "registry-generator-v4", "")
	assertSDKGeneratorRowState(t, ctx, pool, v8ActiveAppID, models.SDKGeneratorVersion, "existing-v8-job")
	assertSDKGeneratorRowState(t, ctx, pool, mcpAppID, "", "")

	again, err := migrator.MigrateSDKGeneratorVersions(ctx, engineID)
	// Repeated Engine startups must be a zero-row no-op once pre-v5 rows are retired and v5-v7 rows are upgraded.
	if err != nil || again.DeletedRows != 0 || again.MigratedRows != 0 || again.ReboundJobIDs != 0 {
		t.Fatalf("second migration = %+v, err = %v", again, err)
	}
}

// insertSDKGeneratorMigrationFamily creates one disposable app family and registers cleanup for the test.
func insertSDKGeneratorMigrationFamily(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID, ownerID uuid.UUID, kind, deliveryMode, name string) uuid.UUID {
	t.Helper()
	familyID := uuid.New()
	var mode, targetLanguage any
	// MCP families must leave delivery_mode and target_language NULL to satisfy family check constraints.
	if deliveryMode != "" {
		mode = deliveryMode
	}
	if kind == "sdk" {
		targetLanguage = "typescript"
	}
	sourceMigrationExec(t, ctx, pool, `
		INSERT INTO fused_app_families (app_family_id, account_id, kind, delivery_mode, canonical_name, display_name, target_language, owner_subject_id)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7)
	`, familyID, accountID, kind, mode, name, targetLanguage, ownerID)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM fused_app_tombstones WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(ctx, `DELETE FROM fused_apps WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(ctx, `DELETE FROM fused_app_families WHERE app_family_id = $1`, familyID)
	})
	return familyID
}

// insertSDKGeneratorMigrationApp creates one immutable app version row with explicit generator and job state for migration verification.
func insertSDKGeneratorMigrationApp(t *testing.T, ctx context.Context, pool *pgxpool.Pool, familyID, accountID, ownerID uuid.UUID, version, generatorVersion, status, genStatus, jobID string) uuid.UUID {
	t.Helper()
	appID := uuid.New()
	sourceHash := strings.Repeat("a", 64)
	sourceMigrationExec(t, ctx, pool, `
		INSERT INTO fused_apps (
			app_id, app_family_id, account_id, version, config_key, source_hash,
			generator_version, sdk_generation_job_id, sdk_generation_status, status, created_by
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, $11
		)
	`, appID, familyID, accountID, version, "migration:"+appID.String(), sourceHash, generatorVersion, jobID, genStatus, status, ownerID)
	return appID
}

// assertSDKGeneratorRowState verifies the persisted generator version and job ID match expectations after migration.
func assertSDKGeneratorRowState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, appID uuid.UUID, wantGenerator, wantJobID string) {
	t.Helper()
	var gotGenerator, gotJobID string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(generator_version, ''), COALESCE(sdk_generation_job_id, '')
		FROM fused_apps
		WHERE app_id = $1
	`, appID).Scan(&gotGenerator, &gotJobID); err != nil {
		t.Fatalf("read migrated app %s: %v", appID, err)
	}
	// Both generator provenance and job binding must match the expected post-migration contract.
	if gotGenerator != wantGenerator || gotJobID != wantJobID {
		t.Fatalf("app %s = generator=%q job=%q, want generator=%q job=%q", appID, gotGenerator, gotJobID, wantGenerator, wantJobID)
	}
}

// assertSDKGeneratorRowDeletedAndTombstoned verifies a retired pre-v5 SDK was removed from fused_apps and recorded in fused_app_tombstones.
func assertSDKGeneratorRowDeletedAndTombstoned(t *testing.T, ctx context.Context, pool *pgxpool.Pool, appID, familyID uuid.UUID, version string) {
	t.Helper()
	var appCount, tombstoneCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM fused_apps WHERE app_id = $1),
			(SELECT COUNT(*) FROM fused_app_tombstones WHERE app_id = $1 AND app_family_id = $2 AND version = $3)
	`, appID, familyID, version).Scan(&appCount, &tombstoneCount); err != nil {
		t.Fatalf("check retired app %s: %v", appID, err)
	}
	// Retired pre-v5 versions must be absent from fused_apps and permanently tombstoned.
	if appCount != 0 || tombstoneCount != 1 {
		t.Fatalf("retired app %s = appCount=%d tombstoneCount=%d, want 0 and 1", appID, appCount, tombstoneCount)
	}
}
