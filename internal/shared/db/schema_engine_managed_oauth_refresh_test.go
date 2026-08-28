package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestManagedOAuthRefreshMigrationPreservesLegacyConnections starts a real v7
// schema without its optional refresh index and proves normal Engine init runs v8.
func TestManagedOAuthRefreshMigrationPreservesLegacyConnections(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedEngineMigrationPool(t, ctx, databaseURL)
	applyCanonicalEngineSchemaForMigrationTest(t, ctx, pool)
	fixture := seedManagedOAuthRefreshLegacyRows(t, ctx, pool)
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("initialize managed OAuth refresh migration: %v", err)
	}
	assertManagedOAuthRefreshMigrationRows(t, ctx, pool, fixture)
	assertManagedOAuthRefreshMigrationLedger(t, ctx, pool)
}

type managedOAuthRefreshMigrationFixture struct {
	uniqueConnectionID    uuid.UUID
	ambiguousConnectionID uuid.UUID
	uniqueVersionID       uuid.UUID
	legacySessionID       uuid.UUID
}

// isolatedEngineMigrationPool creates a UUID-owned schema and drops only that
// schema after a real-PostgreSQL migration test completes.
func isolatedEngineMigrationPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect migration test database: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := "engine_migration_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create managed OAuth refresh test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop managed OAuth refresh test schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse migration test database URL: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated managed OAuth refresh pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyCanonicalEngineSchemaForMigrationTest creates the current tables, then
// records immutable v1-v7 identities so only the new v8 migration executes.
func applyCanonicalEngineSchemaForMigrationTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range engineSchemaQueries() {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("create canonical Engine migration fixture schema: %v", err)
		}
	}
	for _, migration := range engineMigrations()[:7] {
		if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`, migration.Version, migration.Name, time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("seed migration %d identity: %v", migration.Version, err)
		}
	}
	if err := restoreManagedOAuthRefreshV7Shape(ctx, pool); err != nil {
		t.Fatalf("restore managed OAuth refresh v7 shape: %v", err)
	}
}

// restoreManagedOAuthRefreshV7Shape removes every canonical v8 field and the
// optional v7 refresh index so pre-migration startup ordering is exercised.
func restoreManagedOAuthRefreshV7Shape(ctx context.Context, pool *pgxpool.Pool) error {
	queries := []string{
		`ALTER TABLE fused_auth_connections DROP CONSTRAINT IF EXISTS chk_fused_auth_connections_refresh_lease`,
		`DROP INDEX IF EXISTS idx_fused_auth_connections_refresh`,
		`ALTER TABLE fused_auth_connections
			DROP COLUMN service_version_id,
			DROP COLUMN last_refresh_attempt_at,
			DROP COLUMN last_refreshed_at,
			DROP COLUMN refresh_retry_not_before,
			DROP COLUMN refresh_lease_token,
			DROP COLUMN refresh_lease_expires_at`,
		`ALTER TABLE fused_connect_sessions DROP COLUMN service_version_id`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

// seedManagedOAuthRefreshLegacyRows creates one unambiguous and one ambiguous
// service while retaining sentinel credential data for preservation checks.
func seedManagedOAuthRefreshLegacyRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) managedOAuthRefreshMigrationFixture {
	t.Helper()
	fixture := managedOAuthRefreshMigrationFixture{
		uniqueConnectionID: uuid.New(), ambiguousConnectionID: uuid.New(),
		uniqueVersionID: uuid.New(), legacySessionID: uuid.New(),
	}
	uniqueServiceID, ambiguousServiceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, "oauth-refresh-"+bucketID.String()); err != nil {
		t.Fatalf("seed migration bucket: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_name) VALUES ($1, 'Unique'), ($2, 'Ambiguous')`, uniqueServiceID, ambiguousServiceID); err != nil {
		t.Fatalf("seed migration services: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version, status) VALUES
		($1, $2, '1.0.0', 'public'), ($3, $4, '1.0.0', 'public'), ($3, $5, '2.0.0', 'public')`,
		uniqueServiceID, fixture.uniqueVersionID, ambiguousServiceID, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("seed migration service versions: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_auth_connections
		(id, bucket_id, service_id, end_user_ref, auth_type, auth_name,
		 credential_source_service_id, credential_source_auth_type, credential_source_auth_name,
		 encrypted_dek, access_token, refresh_token)
		VALUES ($1, $3, $4, 'unique-user', 'oauth', 'oauth', $4, 'oauth', 'oauth', 'legacy-dek', 'unique-access', 'unique-refresh'),
		       ($2, $3, $5, 'ambiguous-user', 'oauth', 'oauth', $5, 'oauth', 'oauth', 'legacy-dek', 'ambiguous-access', 'ambiguous-refresh')`,
		fixture.uniqueConnectionID, fixture.ambiguousConnectionID, bucketID, uniqueServiceID, ambiguousServiceID); err != nil {
		t.Fatalf("seed legacy auth connections: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_connect_sessions
		(id, bucket_id, service_id, auth_type, auth_name,
		 credential_source_service_id, credential_source_auth_type, credential_source_auth_name,
		 end_user_ref, state_hash, expires_at)
		VALUES ($1, $2, $3, 'oauth', 'oauth', $3, 'oauth', 'oauth', 'legacy-session-user', $4, NOW() + INTERVAL '10 minutes')`,
		fixture.legacySessionID, bucketID, uniqueServiceID, "state-"+uuid.NewString()); err != nil {
		t.Fatalf("seed legacy connect session: %v", err)
	}
	return fixture
}

// assertManagedOAuthRefreshMigrationRows verifies preservation, exact backfill,
// ambiguous fail-closed behavior, and enforcement on post-migration sessions.
func assertManagedOAuthRefreshMigrationRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture managedOAuthRefreshMigrationFixture) {
	t.Helper()
	assertManagedOAuthRefreshConnectionBackfill(t, ctx, pool, fixture)
	assertManagedOAuthRefreshSessionBackfill(t, ctx, pool, fixture)
	assertManagedOAuthRefreshMigrationStructure(t, ctx, pool)
}

// assertManagedOAuthRefreshConnectionBackfill verifies exact version binding
// only for the unambiguous service while preserving both credential sentinels.
func assertManagedOAuthRefreshConnectionBackfill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture managedOAuthRefreshMigrationFixture) {
	t.Helper()
	var uniqueVersion, ambiguousVersion *uuid.UUID
	var uniqueAccess, ambiguousAccess string
	if err := pool.QueryRow(ctx, `SELECT service_version_id, access_token FROM fused_auth_connections WHERE id = $1`, fixture.uniqueConnectionID).Scan(&uniqueVersion, &uniqueAccess); err != nil {
		t.Fatalf("read unique migrated connection: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT service_version_id, access_token FROM fused_auth_connections WHERE id = $1`, fixture.ambiguousConnectionID).Scan(&ambiguousVersion, &ambiguousAccess); err != nil {
		t.Fatalf("read ambiguous migrated connection: %v", err)
	}
	if uniqueVersion == nil || *uniqueVersion != fixture.uniqueVersionID || uniqueAccess != "unique-access" {
		t.Fatalf("unique migrated connection version=%v access=%q", uniqueVersion, uniqueAccess)
	}
	if ambiguousVersion != nil || ambiguousAccess != "ambiguous-access" {
		t.Fatalf("ambiguous migrated connection version=%v access=%q", ambiguousVersion, ambiguousAccess)
	}
}

// assertManagedOAuthRefreshSessionBackfill verifies an unambiguous legacy
// browser session survives while post-migration nil inserts fail closed.
func assertManagedOAuthRefreshSessionBackfill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture managedOAuthRefreshMigrationFixture) {
	t.Helper()
	var sessionVersion *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT service_version_id FROM fused_connect_sessions WHERE id = $1`, fixture.legacySessionID).Scan(&sessionVersion); err != nil || sessionVersion == nil || *sessionVersion != fixture.uniqueVersionID {
		t.Fatalf("legacy session version=%v err=%v", sessionVersion, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_connect_sessions
		(bucket_id, service_id, auth_type, auth_name, end_user_ref, state_hash, expires_at)
		SELECT bucket_id, service_id, auth_type, auth_name, 'new-invalid-user', $2, expires_at
		FROM fused_connect_sessions WHERE id = $1`, fixture.legacySessionID, "state-"+uuid.NewString()); err == nil {
		t.Fatal("post-migration connect session without service_version_id was accepted")
	}
}

// assertManagedOAuthRefreshMigrationStructure verifies v8 physically adds its
// columns, constraints, and OAuth-aware due index to the reconstructed v7 tables.
func assertManagedOAuthRefreshMigrationStructure(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	assertManagedOAuthRefreshMigrationColumns(t, ctx, pool)
	assertManagedOAuthRefreshMigrationConstraints(t, ctx, pool)
	assertManagedOAuthRefreshMigrationIndex(t, ctx, pool)
}

// assertManagedOAuthRefreshMigrationColumns verifies every field was added to
// the reconstructed v7 auth and session tables.
func assertManagedOAuthRefreshMigrationColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var authColumns int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'fused_auth_connections'
		  AND column_name = ANY($1::text[])`, []string{
		"service_version_id", "last_refresh_attempt_at", "last_refreshed_at",
		"refresh_retry_not_before", "refresh_lease_token", "refresh_lease_expires_at",
	}).Scan(&authColumns); err != nil || authColumns != 6 {
		t.Fatalf("managed refresh auth columns=%d err=%v", authColumns, err)
	}
	var sessionColumnCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'fused_connect_sessions'
		  AND column_name = 'service_version_id'`).Scan(&sessionColumnCount); err != nil || sessionColumnCount != 1 {
		t.Fatalf("managed refresh session column count=%d err=%v", sessionColumnCount, err)
	}
}

// assertManagedOAuthRefreshMigrationConstraints checks validated lease pairing
// and intentionally unvalidated legacy-session compatibility independently.
func assertManagedOAuthRefreshMigrationConstraints(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var leaseValidated, sessionValidated bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT convalidated FROM pg_constraint
		 WHERE conname = 'chk_fused_auth_connections_refresh_lease'
		   AND conrelid = 'fused_auth_connections'::regclass),
		(SELECT convalidated FROM pg_constraint
		 WHERE conname = 'chk_fused_connect_sessions_service_version'
		   AND conrelid = 'fused_connect_sessions'::regclass)`).Scan(&leaseValidated, &sessionValidated); err != nil {
		t.Fatalf("read managed refresh constraints: %v", err)
	}
	if !leaseValidated || sessionValidated {
		t.Fatalf("constraint validation lease=%t session=%t", leaseValidated, sessionValidated)
	}
}

// assertManagedOAuthRefreshMigrationIndex verifies worker discovery uses OAuth
// type and retryable-state filtering rather than requiring refresh material.
func assertManagedOAuthRefreshMigrationIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var indexDefinition string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid) FROM pg_index WHERE indexrelid = 'idx_fused_auth_connections_refresh'::regclass`).Scan(&indexDefinition); err != nil {
		t.Fatalf("read managed refresh index: %v", err)
	}
	// The final v18 index must recognize authorization-code aliases through the same normalization used by worker claims.
	if !strings.Contains(indexDefinition, "lower(replace(btrim(auth_type), '-'::text, '_'::text))") ||
		!strings.Contains(indexDefinition, "oauth2_authorization_code") || !strings.Contains(indexDefinition, "failed") ||
		!strings.Contains(indexDefinition, "refresh_token_expires_at") || strings.Contains(indexDefinition, "refresh_token IS NOT NULL") {
		t.Fatalf("managed refresh index definition = %q", indexDefinition)
	}
}

// assertManagedOAuthRefreshMigrationLedger proves all seven previous immutable
// records retain identity/timestamp while v8 remains present after later appends.
func assertManagedOAuthRefreshMigrationLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_engine_schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count Engine migration ledger: %v", err)
	}
	if count != len(engineMigrations()) {
		t.Fatalf("Engine migration ledger count = %d, want %d", count, len(engineMigrations()))
	}
	seededAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, migration := range engineMigrations()[:7] {
		assertManagedOAuthRefreshPriorMigration(t, ctx, pool, migration, seededAt)
	}
	var appendedName string
	if err := pool.QueryRow(ctx, `SELECT name FROM fused_engine_schema_migrations WHERE version = $1`, managedOAuthRefreshMigrationVersion).Scan(&appendedName); err != nil {
		t.Fatalf("read managed refresh migration identity: %v", err)
	}
	if appendedName != managedOAuthRefreshMigrationName {
		t.Fatalf("managed refresh migration name = %q, want %q", appendedName, managedOAuthRefreshMigrationName)
	}
}

// assertManagedOAuthRefreshPriorMigration verifies v8 never rewrites an older
// ledger identity or its original application timestamp.
func assertManagedOAuthRefreshPriorMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, migration engineMigration, seededAt time.Time) {
	t.Helper()
	var name string
	var appliedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT name, applied_at FROM fused_engine_schema_migrations WHERE version = $1`, migration.Version).Scan(&name, &appliedAt); err != nil {
		t.Fatalf("read prior migration %d identity: %v", migration.Version, err)
	}
	if name != migration.Name || !appliedAt.Equal(seededAt) {
		t.Fatalf("prior migration %d changed name=%q applied_at=%s", migration.Version, name, appliedAt)
	}
}
