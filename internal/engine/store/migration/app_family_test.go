package migration

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	enginedb "github.com/Usefused/engine/internal/shared/db"
)

// TestDryRun_EmptyDatabase verifies dry-run produces a valid but empty report
// when no artifact scopes exist.
func TestDryRun_EmptyDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 0, report.TotalArtifactScopes)
	assert.Equal(t, 0, report.TotalFamilies)
}

// TestDryRun_SingleScope verifies a single SDK scope produces one conflict-free family.
func TestDryRun_SingleScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	artifactID := uuid.New()
	ownerSubID := uuid.New()

	insertScope(t, ctx, db, accountID, artifactID, "sdk", "my-sdk", "1.0.0", "sdk:my-sdk:1.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 1, report.TotalArtifactScopes)
	assert.Equal(t, 1, report.TotalFamilies)
	assert.Equal(t, 1, report.ConflictFreeGroups)
	assert.Equal(t, 0, report.ConflictGroups)

	family := report.Families[0]
	assert.Equal(t, "sdk", family.Kind)
	assert.Equal(t, "my-sdk", family.CanonicalName)
	assert.Equal(t, "my-sdk", family.DisplayName)
	assert.Len(t, family.Members, 1)
	assert.Equal(t, artifactID, family.Members[0].AppID)
	assert.Equal(t, "1.0.0", family.Members[0].Version)
	assert.Empty(t, family.Conflicts)
}

// TestDryRun_MultipleVersionsSameFamily verifies two versions of the same SDK
// group under one family.
func TestDryRun_MultipleVersionsSameFamily(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	artifactV1 := uuid.New()
	artifactV2 := uuid.New()
	ownerSubID := uuid.New()

	insertScope(t, ctx, db, accountID, artifactV1, "sdk", "my-sdk", "1.0.0", "sdk:my-sdk:1.0.0", &ownerSubID, nil)
	insertScope(t, ctx, db, accountID, artifactV2, "sdk", "my-sdk", "2.0.0", "sdk:my-sdk:2.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 2, report.TotalArtifactScopes)
	assert.Equal(t, 1, report.TotalFamilies)
	assert.Equal(t, 1, report.ConflictFreeGroups)

	family := report.Families[0]
	assert.Len(t, family.Members, 2)
}

// TestDryRun_SDKandMCPSameNameDifferentFamilies verifies that an SDK and MCP
// with the same name remain separate families (different kinds).
func TestDryRun_SDKandMCPDifferentFamilies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	sdkID := uuid.New()
	mcpID := uuid.New()
	ownerSubID := uuid.New()

	insertScope(t, ctx, db, accountID, sdkID, "sdk", "my-app", "1.0.0", "sdk:my-app:1.0.0", &ownerSubID, nil)
	insertScope(t, ctx, db, accountID, mcpID, "mcp", "my-app", "1.0.0", "mcp:my-app:1.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 2, report.TotalArtifactScopes)
	assert.Equal(t, 2, report.TotalFamilies)
	assert.Equal(t, 2, report.ConflictFreeGroups)
}

// TestDryRun_UnicodeNormalization verifies that Unicode variants of the same
// name group together.
func TestDryRun_UnicodeNormalization(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	a1 := uuid.New()
	a2 := uuid.New()
	ownerSubID := uuid.New()

	// Precomposed vs decomposed: "café" vs "cafe\u0301"
	insertScope(t, ctx, db, accountID, a1, "sdk", "café", "1.0.0", "sdk:café:1.0.0", &ownerSubID, nil)
	insertScope(t, ctx, db, accountID, a2, "sdk", "cafe\u0301", "1.0.0", "sdk:cafe\u0301:1.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 1, report.TotalFamilies)
	assert.Len(t, report.Families[0].Members, 2)
}

// TestDryRun_CaseInsensitiveGrouping verifies that case variants group together.
func TestDryRun_CaseInsensitiveGrouping(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	a1 := uuid.New()
	a2 := uuid.New()
	ownerSubID := uuid.New()

	insertScope(t, ctx, db, accountID, a1, "sdk", "MySDK", "1.0.0", "sdk:MySDK:1.0.0", &ownerSubID, nil)
	insertScope(t, ctx, db, accountID, a2, "sdk", "mysdk", "2.0.0", "sdk:mysdk:2.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 1, report.TotalFamilies)
	assert.Len(t, report.Families[0].Members, 2)
}

// TestDryRun_DifferentAccountsNeverMerge verifies scopes from different
// accounts remain separate families even with identical names.
func TestDryRun_DifferentAccountsNeverMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	account1 := uuid.New()
	account2 := uuid.New()
	a1 := uuid.New()
	a2 := uuid.New()
	ownerSubID := uuid.New()

	insertScope(t, ctx, db, account1, a1, "sdk", "my-sdk", "1.0.0", "sdk:my-sdk:1.0.0", &ownerSubID, nil)
	insertScope(t, ctx, db, account2, a2, "sdk", "MY-SDK", "1.0.0", "sdk:MY-SDK:1.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 2, report.TotalFamilies)
}

// TestDryRun_OwnerMismatchConflict verifies that different owners within a
// group produce a conflict.
func TestDryRun_OwnerMismatchConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	a1 := uuid.New()
	a2 := uuid.New()
	owner1 := uuid.New()
	owner2 := uuid.New()

	insertScope(t, ctx, db, accountID, a1, "sdk", "my-sdk", "1.0.0", "sdk:my-sdk:1.0.0", &owner1, nil)
	insertScope(t, ctx, db, accountID, a2, "sdk", "my-sdk", "2.0.0", "sdk:my-sdk:2.0.0", &owner2, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 0, report.ConflictFreeGroups)
	assert.Equal(t, 1, report.ConflictGroups)
	assert.NotEmpty(t, report.Families[0].Conflicts)
}

// TestDryRun_MissingNameUnresolved verifies scopes without a name are reported
// as unresolved.
func TestDryRun_MissingNameUnresolved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	artifactID := uuid.New()
	ownerSubID := uuid.New()

	insertScopeWithoutName(t, ctx, db, accountID, artifactID, "sdk", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 1, report.UnresolvedScopes)
}

// TestDryRun_ColonInNamePreserved verifies names containing colons are handled
// correctly (not split).
func TestDryRun_ColonInNamePreserved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	a1 := uuid.New()
	a2 := uuid.New()
	ownerSubID := uuid.New()

	// Names containing colons should still group if they match exactly.
	insertScope(t, ctx, db, accountID, a1, "sdk", "jira:plunk-sdk", "1.0.0", "sdk:jira:plunk-sdk:1.0.0", &ownerSubID, nil)
	insertScope(t, ctx, db, accountID, a2, "sdk", "jira:plunk-sdk", "2.0.0", "sdk:jira:plunk-sdk:2.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 1, report.TotalFamilies)
	assert.Equal(t, "jira:plunk-sdk", report.Families[0].CanonicalName)
	assert.Len(t, report.Families[0].Members, 2)
}

// TestDryRun_ReportJSON verifies the report marshals to valid JSON.
func TestDryRun_ReportJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()

	accountID := uuid.New()
	artifactID := uuid.New()
	ownerSubID := uuid.New()

	insertScope(t, ctx, db, accountID, artifactID, "sdk", "my-sdk", "1.0.0", "sdk:my-sdk:1.0.0", &ownerSubID, nil)

	report, err := DryRun(ctx, db)
	require.NoError(t, err)

	data, err := report.ToJSON()
	require.NoError(t, err)

	var parsed DryRunReport
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, report.TotalArtifactScopes, parsed.TotalArtifactScopes)
	assert.Equal(t, report.TotalFamilies, parsed.TotalFamilies)
}

func TestApplyPreservesVersionTokenAndBucketIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	ctx := context.Background()
	accountID, artifactID := uuid.New(), uuid.New()
	ownerSubjectID, bucketID := uuid.New(), uuid.New()
	insertScope(t, ctx, db, accountID, artifactID, "sdk", "jira:plunk", "1.0.0",
		"sdk:jira:plunk:1.0.0", &ownerSubjectID, nil)
	_, err := db.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, "migration-"+bucketID.String())
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO fused_artifact_buckets (artifact_id, bucket_id) VALUES ($1, $2)`, artifactID, bucketID)
	require.NoError(t, err)
	tokenID, tokenHash := uuid.New(), "existing-token-"+uuid.NewString()
	_, err = db.Exec(ctx, `
		INSERT INTO fused_artifact_tokens (id, artifact_id, token_hash, name)
		VALUES ($1, $2, $3, 'default')
	`, tokenID, artifactID, tokenHash)
	require.NoError(t, err)

	report, err := Apply(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 1, report.AppliedFamilies)
	assert.Equal(t, 1, report.MigratedApps)
	assert.Equal(t, 1, report.MigratedTokens)

	var appID, familyID, migratedTokenID, migratedBucketID uuid.UUID
	var migratedHash string
	require.NoError(t, db.QueryRow(ctx, `
		SELECT app.app_id, app.app_family_id, token.id, token.token_hash, binding.bucket_id
		FROM fused_apps app
		JOIN fused_app_tokens token ON token.app_family_id = app.app_family_id
		JOIN fused_app_family_buckets binding ON binding.app_family_id = app.app_family_id
		WHERE app.app_id = $1
	`, artifactID).Scan(&appID, &familyID, &migratedTokenID, &migratedHash, &migratedBucketID))
	assert.Equal(t, artifactID, appID)
	assert.NotEqual(t, uuid.Nil, familyID)
	assert.Equal(t, tokenID, migratedTokenID)
	assert.Equal(t, tokenHash, migratedHash)
	assert.Equal(t, bucketID, migratedBucketID)

	for _, table := range []string{"fused_artifact_scopes", "fused_artifact_tokens", "fused_artifact_buckets", "fused_artifact_snapshots"} {
		var present *string
		require.NoError(t, db.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&present))
		assert.Nil(t, present, "legacy table %s must be removed after the validated cutover", table)
	}
}

// --- helpers ---

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connStr := lookupEnv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL is required for destructive migration integration tests")
	}
	parsed, err := url.Parse(connStr)
	if err != nil || !strings.Contains(strings.ToLower(parsed.Path), "test") {
		t.Fatal("TEST_DATABASE_URL must name a dedicated test database")
	}
	initialized, err := enginedb.InitEnginePostgres(context.Background(), connStr)
	if err != nil {
		t.Fatalf("initialize target app schema: %v", err)
	}
	initialized.Close()

	pool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	ensureLegacyMigrationTables(t, pool)

	// These tests require an isolated database because migration validation must
	// exercise real foreign keys and set-based writes, not mocked SQL behavior.
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_family_migrations`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_tokens`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_family_buckets`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_tombstones`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_config_states`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_artifact_buckets`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_artifact_tokens`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM fused_artifact_scopes`)

	return pool
}

func ensureLegacyMigrationTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	queries := []string{
		`CREATE TABLE IF NOT EXISTS fused_artifact_scopes (
			account_id uuid NOT NULL,
			artifact_id uuid PRIMARY KEY,
			owner_subject_id uuid,
			owner_team_id uuid,
			kind text NOT NULL,
			name text,
			version text,
			config_key text,
			scope_schema_version integer NOT NULL DEFAULT 1,
			selections jsonb NOT NULL DEFAULT '[]'::jsonb,
			deactivated_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS fused_artifact_tokens (
			id uuid PRIMARY KEY,
			artifact_id uuid NOT NULL REFERENCES fused_artifact_scopes(artifact_id) ON DELETE CASCADE,
			token_hash text NOT NULL UNIQUE,
			name text NOT NULL,
			last_used_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS fused_artifact_buckets (
			artifact_id uuid PRIMARY KEY REFERENCES fused_artifact_scopes(artifact_id) ON DELETE CASCADE,
			bucket_id uuid NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS fused_artifact_snapshots (
			artifact_id uuid PRIMARY KEY
		);`,
	}
	for _, query := range queries {
		_, err := pool.Exec(context.Background(), query)
		require.NoError(t, err)
	}
}

func lookupEnv(key string) string {
	return os.Getenv(key)
}

func insertScope(t *testing.T, ctx context.Context, db *pgxpool.Pool,
	accountID, artifactID uuid.UUID, kind, name, version, configKey string,
	ownerSubjectID, ownerTeamID *uuid.UUID,
) {
	t.Helper()
	insertTestOwners(t, ctx, db, ownerSubjectID, ownerTeamID)
	_, err := db.Exec(ctx, `
		INSERT INTO fused_artifact_scopes
			(account_id, artifact_id, kind, name, version, config_key,
			 owner_subject_id, owner_team_id, selections)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '[]'::jsonb)
	`, accountID, artifactID, kind, name, version, configKey,
		ownerSubjectID, ownerTeamID)
	require.NoError(t, err, "insertScope")

	desired := map[string]any{
		"kind":    kind,
		"name":    name,
		"version": version,
	}
	if kind == "sdk" {
		desired["language"] = "typescript"
	}
	desiredJSON, err := json.Marshal(desired)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `
		INSERT INTO fused_config_states
			(config_key, config_type, owner_subject_id, owner_team_id, source_hash,
			 desired_state, latest_resource_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, configKey, kind, ownerSubjectID, ownerTeamID,
		"source-"+artifactID.String(), desiredJSON, artifactID)
	require.NoError(t, err, "insert config state")
}

func insertScopeWithoutName(t *testing.T, ctx context.Context, db *pgxpool.Pool,
	accountID, artifactID uuid.UUID, kind string,
	ownerSubjectID, ownerTeamID *uuid.UUID,
) {
	t.Helper()
	insertTestOwners(t, ctx, db, ownerSubjectID, ownerTeamID)
	_, err := db.Exec(ctx, `
		INSERT INTO fused_artifact_scopes
			(account_id, artifact_id, kind, name, selections,
			 owner_subject_id, owner_team_id)
		VALUES ($1, $2, $3, NULL, '[]'::jsonb, $4, $5)
	`, accountID, artifactID, kind, ownerSubjectID, ownerTeamID)
	require.NoError(t, err, "insertScopeWithoutName")
}

func insertTestOwners(
	t *testing.T,
	ctx context.Context,
	db *pgxpool.Pool,
	ownerSubjectID, ownerTeamID *uuid.UUID,
) {
	t.Helper()
	if ownerSubjectID != nil {
		_, err := db.Exec(ctx, `
			INSERT INTO fused_subjects (id, kind, display_name)
			VALUES ($1, 'user', 'Migration test owner')
			ON CONFLICT (id) DO NOTHING
		`, *ownerSubjectID)
		require.NoError(t, err)
	}
	if ownerTeamID != nil {
		_, err := db.Exec(ctx, `
			INSERT INTO fused_teams (id, name, slug)
			VALUES ($1, 'Migration test team', $2)
			ON CONFLICT (id) DO NOTHING
		`, *ownerTeamID, "migration-test-"+ownerTeamID.String())
		require.NoError(t, err)
	}
}
