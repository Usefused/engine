package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type connectBrandColorMigrationState struct {
	color      string
	customized bool
}

// TestConnectBrandColorMigrationPreservesExplicitChoices proves the version-six
// data migration converges only the legacy default while retaining audited and
// visibly customized colour choices.
func TestConnectBrandColorMigrationPreservesExplicitChoices(t *testing.T) {
	databaseURL := os.Getenv("CONNECT_BRANDING_TEST_DATABASE_URL")
	// Missing isolation configuration skips the real-database proof instead of
	// falling back to any generic Engine database.
	if databaseURL == "" {
		// A dedicated variable keeps migration testing away from an Engine that
		// may contain live OAuth connections or customer branding.
		t.Skip("CONNECT_BRANDING_TEST_DATABASE_URL is required for isolated PostgreSQL coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	// Connection failures identify the disposable database setup as incomplete.
	if err != nil {
		t.Fatalf("connect isolated PostgreSQL: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	// A transaction is mandatory because every migration fixture is rolled back.
	if err != nil {
		t.Fatalf("begin brand-colour migration fixture: %v", err)
	}
	// The rollback and temporary tables make the fixture non-destructive even
	// when the dedicated database is reused across local test runs.
	defer func() { _ = tx.Rollback(ctx) }()

	createConnectBrandColorMigrationFixture(t, ctx, tx)
	applyConnectBrandColorMigration(t, ctx, tx)
	assertConnectBrandColorMigrationStates(t, ctx, tx)
}

// TestConnectBrandVioletMigrationPreservesExplicitBlueChoice proves the
// additive migration changes only the prior generated default and insertion default.
func TestConnectBrandVioletMigrationPreservesExplicitBlueChoice(t *testing.T) {
	databaseURL := os.Getenv("CONNECT_BRANDING_TEST_DATABASE_URL")
	// A dedicated database is required because the fixture shadows production table names.
	if databaseURL == "" {
		t.Skip("CONNECT_BRANDING_TEST_DATABASE_URL is required for isolated PostgreSQL coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect isolated PostgreSQL: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin brand-violet migration fixture: %v", err)
	}
	// Transaction rollback makes the migration proof safe for a reused test database.
	defer func() { _ = tx.Rollback(ctx) }()

	createConnectBrandVioletMigrationFixture(t, ctx, tx)
	applyConnectBrandVioletMigration(t, ctx, tx)
	assertConnectBrandVioletMigrationStates(t, ctx, tx)
}

// createConnectBrandColorMigrationFixture shadows production table names with
// transaction-local tables representing untouched, audited, and visible edits.
func createConnectBrandColorMigrationFixture(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	// The workspace fixture begins at the exact version-five column shape.
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE fused_workspaces (
		id uuid PRIMARY KEY,
		connect_primary_color text NOT NULL DEFAULT '#18181b'
	) ON COMMIT DROP`); err != nil {
		t.Fatalf("create legacy workspace fixture: %v", err)
	}
	// The audit fixture retains only the bounded fields consulted by migration.
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE fused_audit_events (
		resource_type text,
		resource_id uuid,
		action text NOT NULL,
		path text NOT NULL,
		permission text,
		outcome text NOT NULL,
		metadata jsonb NOT NULL DEFAULT '{}'::jsonb
	) ON COMMIT DROP`); err != nil {
		t.Fatalf("create branding audit fixture: %v", err)
	}
	// Six rows isolate untouched black, chosen black, a distinct colour, and three rejected audit shapes.
	if _, err := tx.Exec(ctx, `INSERT INTO fused_workspaces (id, connect_primary_color) VALUES
		('00000000-0000-0000-0000-000000000101', '#18181b'),
		('00000000-0000-0000-0000-000000000102', '#18181b'),
		('00000000-0000-0000-0000-000000000103', '#123456'),
		('00000000-0000-0000-0000-000000000104', '#18181b'),
		('00000000-0000-0000-0000-000000000105', '#18181b'),
		('00000000-0000-0000-0000-000000000106', '#18181b')`); err != nil {
		t.Fatalf("seed legacy workspace branding: %v", err)
	}
	// Only the exact successful route audit proves a choice; failed, other-path,
	// and other-permission events must leave the legacy default eligible.
	if _, err := tx.Exec(ctx, `INSERT INTO fused_audit_events (resource_type, resource_id, action, path, permission, outcome, metadata) VALUES
		('workspace', '00000000-0000-0000-0000-000000000102', 'control.http.put', '/workspace/connect-branding', 'workspace.update', 'succeeded', '{"primary_color_changed": true}'),
		('workspace', '00000000-0000-0000-0000-000000000104', 'control.http.put', '/workspace/connect-branding', 'workspace.update', 'failed', '{"primary_color_changed": true}'),
		('workspace', '00000000-0000-0000-0000-000000000105', 'control.http.put', '/workspace', 'workspace.update', 'succeeded', '{"primary_color_changed": true}'),
		('workspace', '00000000-0000-0000-0000-000000000106', 'control.http.put', '/workspace/connect-branding', 'workspace.read', 'succeeded', '{"primary_color_changed": true}')`); err != nil {
		t.Fatalf("seed explicit legacy colour audit: %v", err)
	}
}

// applyConnectBrandColorMigration executes the immutable version-six sequence
// against the temporary legacy shape in its declared order.
func applyConnectBrandColorMigration(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	// Declared order classifies protected rows before replacing legacy defaults.
	for _, query := range connectBrandColorMigrationQueries() {
		// Reporting the exact statement makes an ordering or PostgreSQL syntax
		// regression diagnosable without exposing any customer-supplied value.
		if _, err := tx.Exec(ctx, query); err != nil {
			t.Fatalf("apply connect brand-colour migration %q: %v", query, err)
		}
	}
}

// assertConnectBrandColorMigrationStates verifies convergence, preservation,
// and the post-migration insertion default with constant-query reads.
func assertConnectBrandColorMigrationStates(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	wants := map[string]connectBrandColorMigrationState{
		"00000000-0000-0000-0000-000000000101": {color: "#2563eb", customized: false},
		"00000000-0000-0000-0000-000000000102": {color: "#18181b", customized: true},
		"00000000-0000-0000-0000-000000000103": {color: "#123456", customized: true},
		"00000000-0000-0000-0000-000000000104": {color: "#2563eb", customized: false},
		"00000000-0000-0000-0000-000000000105": {color: "#2563eb", customized: false},
		"00000000-0000-0000-0000-000000000106": {color: "#2563eb", customized: false},
	}
	rows, err := tx.Query(ctx, `SELECT id::text, connect_primary_color, connect_primary_color_customized FROM fused_workspaces`)
	// A single read verifies every fixture without per-row queries.
	if err != nil {
		t.Fatalf("read migrated brand colours: %v", err)
	}
	defer rows.Close()
	// Each returned row is compared to its keyed expectation without follow-up SQL.
	for rows.Next() {
		var id string
		var got connectBrandColorMigrationState
		// Scan failures are reported before a partial state can be accepted.
		if err := rows.Scan(&id, &got.color, &got.customized); err != nil {
			t.Fatalf("scan migrated brand colour: %v", err)
		}
		want, ok := wants[id]
		// Every fixture row must retain its own independently expected state.
		if !ok || got != want {
			t.Fatalf("migrated brand colour %s = %#v, want %#v", id, got, want)
		}
		delete(wants, id)
	}
	// Iterator errors take precedence over the completeness assertion below.
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated brand colours: %v", err)
	}
	// Missing rows would hide a destructive migration behind otherwise valid values.
	if len(wants) != 0 {
		t.Fatalf("brand-colour migration lost fixtures: %#v", wants)
	}
	var inserted connectBrandColorMigrationState
	// A post-migration insert proves the ALTER DEFAULT result, not just updates.
	if err := tx.QueryRow(ctx, `INSERT INTO fused_workspaces (id) VALUES ('00000000-0000-0000-0000-000000000107') RETURNING connect_primary_color, connect_primary_color_customized`).Scan(&inserted.color, &inserted.customized); err != nil {
		t.Fatalf("insert workspace with migrated defaults: %v", err)
	}
	// Fresh rows must remain uncustomized so future default migrations can converge them.
	if want := (connectBrandColorMigrationState{color: "#2563eb", customized: false}); inserted != want {
		t.Fatalf("post-migration brand colour default = %#v, want %#v", inserted, want)
	}
}

// createConnectBrandVioletMigrationFixture represents generated blue, chosen
// blue, and a distinct explicit customer colour after migration version six.
func createConnectBrandVioletMigrationFixture(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE fused_workspaces (
		id uuid PRIMARY KEY,
		connect_primary_color text NOT NULL DEFAULT '#2563eb',
		connect_primary_color_customized boolean NOT NULL DEFAULT false
	) ON COMMIT DROP`); err != nil {
		t.Fatalf("create prior brand-colour fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fused_workspaces (id, connect_primary_color, connect_primary_color_customized) VALUES
		('00000000-0000-0000-0000-000000000201', '#2563eb', false),
		('00000000-0000-0000-0000-000000000202', '#2563eb', true),
		('00000000-0000-0000-0000-000000000203', '#123456', true)`); err != nil {
		t.Fatalf("seed prior brand-colour fixture: %v", err)
	}
}

// applyConnectBrandVioletMigration executes the immutable version-seven sequence.
func applyConnectBrandVioletMigration(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	for _, query := range connectBrandVioletMigrationQueries() {
		// The exact query identifies any PostgreSQL or ordering regression without row data.
		if _, err := tx.Exec(ctx, query); err != nil {
			t.Fatalf("apply connect brand-violet migration %q: %v", query, err)
		}
	}
}

// assertConnectBrandVioletMigrationStates verifies row preservation and the new default.
func assertConnectBrandVioletMigrationStates(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	rows, err := tx.Query(ctx, `SELECT id::text, connect_primary_color, connect_primary_color_customized FROM fused_workspaces ORDER BY id`)
	if err != nil {
		t.Fatalf("read migrated brand-violet colours: %v", err)
	}
	defer rows.Close()
	wants := []connectBrandColorMigrationState{
		{color: "#6941ff", customized: false},
		{color: "#2563eb", customized: true},
		{color: "#123456", customized: true},
	}
	seen := 0
	for rows.Next() {
		var id string
		var got connectBrandColorMigrationState
		if err := rows.Scan(&id, &got.color, &got.customized); err != nil {
			t.Fatalf("scan migrated brand-violet colour: %v", err)
		}
		if seen >= len(wants) || got != wants[seen] {
			t.Fatalf("migrated brand-violet colour %s = %#v", id, got)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated brand-violet colours: %v", err)
	}
	if seen != len(wants) {
		t.Fatalf("migrated brand-violet row count = %d, want %d", seen, len(wants))
	}
	var inserted connectBrandColorMigrationState
	if err := tx.QueryRow(ctx, `INSERT INTO fused_workspaces (id) VALUES ('00000000-0000-0000-0000-000000000204') RETURNING connect_primary_color, connect_primary_color_customized`).Scan(&inserted.color, &inserted.customized); err != nil {
		t.Fatalf("insert workspace with brand-violet defaults: %v", err)
	}
	if want := (connectBrandColorMigrationState{color: "#6941ff", customized: false}); inserted != want {
		t.Fatalf("post-migration brand-violet default = %#v, want %#v", inserted, want)
	}
}
