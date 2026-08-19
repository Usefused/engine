package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type unifiedConvergenceConstraintIDs struct {
	shape int64
	hash  int64
}

type unifiedConvergenceRow struct {
	marker         string
	schemaVersion  int
	definitions    string
	definitionHash string
	descriptorHash string
}

// TestUnifiedSchemaConvergencePreservesExistingApps proves live pre-Unified
// tables reach the final v2 shape without losing rows or recording a migration.
func TestUnifiedSchemaConvergencePreservesExistingApps(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		// A real PostgreSQL catalog is required to prove idempotent DDL behavior.
		t.Skip("DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Unified convergence fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	createPreUnifiedAppFixture(t, ctx, tx)
	applyUnifiedConvergence(t, ctx, tx)
	firstConstraints := readUnifiedConstraintIDs(t, ctx, tx)
	applyUnifiedConvergence(t, ctx, tx)
	secondConstraints := readUnifiedConstraintIDs(t, ctx, tx)

	assertUnifiedConstraintIdentity(t, firstConstraints, secondConstraints)
	assertUnifiedConvergedRows(t, ctx, tx)
	assertUnifiedConstraintsEnforced(t, ctx, tx)
	assertNoUnifiedMigrationRecord(t, ctx, tx)
}

// createPreUnifiedAppFixture builds the live schema state that predates all
// Unified columns while retaining an existing app row and migration ledger.
func createPreUnifiedAppFixture(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE fused_apps (
		app_id uuid PRIMARY KEY,
		marker text NOT NULL
	) ON COMMIT DROP`); err != nil {
		t.Fatalf("create pre-Unified fused_apps: %v", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE fused_engine_schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL UNIQUE,
		applied_at timestamptz NOT NULL DEFAULT NOW()
	) ON COMMIT DROP`); err != nil {
		t.Fatalf("create migration ledger fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fused_engine_schema_migrations (version, name) VALUES
		(1, '20260810_engine_schema_convergence'),
		(2, '20260810_app_token_policy'),
		(3, '20260811_execution_contract_envelope'),
		(4, '20260811_idempotency_response_media')`); err != nil {
		t.Fatalf("seed migration ledger fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fused_apps (app_id, marker)
		VALUES ('00000000-0000-0000-0000-000000000101', 'keep-me')`); err != nil {
		t.Fatalf("seed pre-Unified app: %v", err)
	}
}

// applyUnifiedConvergence executes the fixed canonical DDL sequence against a
// single PostgreSQL session so temporary fixture tables shadow public tables.
func applyUnifiedConvergence(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	for _, query := range unifiedSchemaConvergenceQueries() {
		if _, err := tx.Exec(ctx, query); err != nil {
			t.Fatalf("apply Unified schema convergence %q: %v", query, err)
		}
	}
}

// readUnifiedConstraintIDs reads both constraint identities in one query so a
// second convergence pass can prove neither constraint was replaced.
func readUnifiedConstraintIDs(t *testing.T, ctx context.Context, tx pgx.Tx) unifiedConvergenceConstraintIDs {
	t.Helper()
	var ids unifiedConvergenceConstraintIDs
	const query = `SELECT
		COALESCE(MAX(oid::bigint) FILTER (WHERE conname = 'chk_fused_apps_unified_definition_shape'), 0),
		COALESCE(MAX(oid::bigint) FILTER (WHERE conname = 'chk_fused_apps_unified_hashes'), 0)
	FROM pg_constraint
	WHERE conrelid = 'fused_apps'::regclass`
	if err := tx.QueryRow(ctx, query).Scan(&ids.shape, &ids.hash); err != nil {
		t.Fatalf("read Unified constraint identities: %v", err)
	}
	return ids
}

// assertUnifiedConstraintIdentity verifies repeated convergence leaves the
// validated constraints intact instead of dropping and revalidating them.
func assertUnifiedConstraintIdentity(t *testing.T, first, second unifiedConvergenceConstraintIDs) {
	t.Helper()
	if first.shape == 0 || first.hash == 0 {
		t.Fatalf("Unified convergence constraint IDs = %+v, want both present", first)
	}
	if first != second {
		t.Fatalf("Unified convergence replaced constraints: first=%+v second=%+v", first, second)
	}
}

// assertUnifiedConvergedRows checks both the preserved row and a post-convergence
// insert receive the canonical v2 defaults without application-side backfill.
func assertUnifiedConvergedRows(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	assertUnifiedConvergedRow(t, ctx, tx, "00000000-0000-0000-0000-000000000101", "keep-me")
	assertUnifiedAppCount(t, ctx, tx, 1)
	if _, err := tx.Exec(ctx, `INSERT INTO fused_apps (app_id, marker)
		VALUES ('00000000-0000-0000-0000-000000000102', 'new-row')`); err != nil {
		t.Fatalf("insert app with converged defaults: %v", err)
	}
	assertUnifiedConvergedRow(t, ctx, tx, "00000000-0000-0000-0000-000000000102", "new-row")
	assertUnifiedAppCount(t, ctx, tx, 2)
}

// assertUnifiedAppCount proves convergence preserves the live row count before
// the test deliberately adds a post-convergence row.
func assertUnifiedAppCount(t *testing.T, ctx context.Context, tx pgx.Tx, want int) {
	t.Helper()
	var rowCount int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM fused_apps`).Scan(&rowCount); err != nil {
		t.Fatalf("count converged app rows: %v", err)
	}
	if rowCount != want {
		t.Fatalf("converged app row count = %d, want %d", rowCount, want)
	}
}

// assertUnifiedConvergedRow verifies one app retains its marker and exposes the
// exact schema-version, empty definition, and hash defaults.
func assertUnifiedConvergedRow(t *testing.T, ctx context.Context, tx pgx.Tx, appID, marker string) {
	t.Helper()
	var row unifiedConvergenceRow
	const query = `SELECT marker, unified_definition_schema_version,
		unified_definitions::text, unified_definition_hash,
		unified_codegen_descriptor_hash
	FROM fused_apps
	WHERE app_id = $1`
	if err := tx.QueryRow(ctx, query, appID).Scan(
		&row.marker,
		&row.schemaVersion,
		&row.definitions,
		&row.definitionHash,
		&row.descriptorHash,
	); err != nil {
		t.Fatalf("read converged app %s: %v", appID, err)
	}
	if row.marker != marker || row.schemaVersion != 2 || row.definitions != "[]" {
		t.Fatalf("converged app %s state = %+v, want marker=%q schema=2 definitions=[]", appID, row, marker)
	}
	if row.definitionHash != unifiedEmptySetHash || row.descriptorHash != unifiedEmptySetHash {
		t.Fatalf("converged app %s hashes = (%q, %q), want empty-set hash", appID, row.definitionHash, row.descriptorHash)
	}
}

// assertUnifiedConstraintsEnforced proves PostgreSQL rejects each invalid
// final-v2 shape while allowing the surrounding test transaction to continue.
func assertUnifiedConstraintsEnforced(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	assertUnifiedWriteRejected(t, ctx, tx, `INSERT INTO fused_apps (
		app_id, marker, unified_definition_schema_version
	) VALUES ('00000000-0000-0000-0000-000000000103', 'bad-version', 1)`)
	assertUnifiedWriteRejected(t, ctx, tx, `INSERT INTO fused_apps (
		app_id, marker, unified_definitions
	) VALUES ('00000000-0000-0000-0000-000000000104', 'bad-definitions', '{}'::jsonb)`)
	assertUnifiedWriteRejected(t, ctx, tx, `INSERT INTO fused_apps (
		app_id, marker, unified_definition_hash
	) VALUES ('00000000-0000-0000-0000-000000000105', 'bad-hash', 'invalid')`)
}

// assertUnifiedWriteRejected isolates an expected constraint failure behind a
// savepoint so all invalid-shape cases run in one database transaction.
func assertUnifiedWriteRejected(t *testing.T, ctx context.Context, tx pgx.Tx, query string) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SAVEPOINT unified_invalid_write"); err != nil {
		t.Fatalf("create invalid-write savepoint: %v", err)
	}
	_, writeErr := tx.Exec(ctx, query)
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT unified_invalid_write"); err != nil {
		t.Fatalf("recover invalid-write savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT unified_invalid_write"); err != nil {
		t.Fatalf("release invalid-write savepoint: %v", err)
	}
	if writeErr == nil {
		t.Fatalf("Unified constraint accepted invalid write %q", query)
	}
}

// assertNoUnifiedMigrationRecord verifies canonical convergence does not claim
// a migration version or name in the immutable Engine ledger.
func assertNoUnifiedMigrationRecord(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	var rows int
	const query = `SELECT COUNT(*)
	FROM fused_engine_schema_migrations
	WHERE version IN (5, 6) OR name ILIKE '%unified%'`
	if err := tx.QueryRow(ctx, query).Scan(&rows); err != nil {
		t.Fatalf("read Unified migration records: %v", err)
	}
	if rows != 0 {
		t.Fatalf("Unified migration ledger rows = %d, want 0", rows)
	}
}
