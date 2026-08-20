package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRESTExecutionMigrationUpgradesV8Receipts proves a real PostgreSQL v8
// database reaches v9 without rewriting its immutable migration ledger.
func TestRESTExecutionMigrationUpgradesV8Receipts(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedEngineMigrationPool(t, ctx, databaseURL)
	seedRESTExecutionV8Schema(t, ctx, pool)
	legacyEventID := insertRESTExecutionEvent(t, ctx, pool, uuid.Nil, uuid.Nil, "")
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("initialize REST execution migration: %v", err)
	}
	assertRESTExecutionV9Constraint(t, ctx, pool, legacyEventID)
	assertRESTExecutionMigrationLedger(t, ctx, pool)
}

// seedRESTExecutionV8Schema creates the canonical tables, records v1-v8, and
// restores the exact receipt constraint shape that existed before REST.
func seedRESTExecutionV8Schema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range engineSchemaQueries() {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("create REST migration fixture schema: %v", err)
		}
	}
	seededAt := restExecutionMigrationSeedTime()
	for _, migration := range engineMigrations()[:8] {
		if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`, migration.Version, migration.Name, seededAt); err != nil {
			t.Fatalf("seed migration %d identity: %v", migration.Version, err)
		}
	}
	queries := []string{
		`ALTER TABLE fused_engine_execution_events DROP CONSTRAINT chk_fused_execution_app_identity`,
		`ALTER TABLE fused_engine_execution_events ADD CONSTRAINT chk_fused_execution_app_identity CHECK (
			transport NOT IN ('sdk', 'mcp') OR (
				app_family_id IS NOT NULL AND app_id IS NOT NULL
				AND NULLIF(BTRIM(app_version), '') IS NOT NULL
			)
		)`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("restore REST migration v8 constraint: %v", err)
		}
	}
}

// insertRESTExecutionEvent inserts the minimal compact receipt shape and
// returns its identity for preservation assertions.
func insertRESTExecutionEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, familyID, appID uuid.UUID, version string) uuid.UUID {
	t.Helper()
	eventID := uuid.New()
	var familyValue, appValue any
	if familyID != uuid.Nil {
		familyValue = familyID
	}
	if appID != uuid.Nil {
		appValue = appID
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_execution_events (
		id, app_family_id, app_id, app_version, transport, endpoint_name,
		status, latency_ms, started_at, ended_at
	) VALUES ($1, $2, $3, $4, 'rest', 'createIssue', 'succeeded', 12, NOW(), NOW())`,
		eventID, familyValue, appValue, version); err != nil {
		t.Fatalf("insert REST execution event: %v", err)
	}
	return eventID
}

// assertRESTExecutionV9Constraint proves v9 preserves a legacy event, accepts
// an exact app receipt, and rejects every newly invalid REST receipt.
func assertRESTExecutionV9Constraint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, legacyEventID uuid.UUID) {
	t.Helper()
	var definition string
	var validated bool
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid), convalidated
		FROM pg_constraint
		WHERE conname = 'chk_fused_execution_app_identity'
		  AND conrelid = 'fused_engine_execution_events'::regclass`).Scan(&definition, &validated); err != nil {
		t.Fatalf("read REST execution constraint: %v", err)
	}
	if !strings.Contains(definition, "'rest'::text") || validated {
		t.Fatalf("REST execution constraint definition=%q validated=%t", definition, validated)
	}
	var legacyCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_engine_execution_events WHERE id = $1`, legacyEventID).Scan(&legacyCount); err != nil || legacyCount != 1 {
		t.Fatalf("legacy REST receipt count=%d err=%v", legacyCount, err)
	}
	insertRESTExecutionEvent(t, ctx, pool, uuid.New(), uuid.New(), "1.0.0")
	if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_execution_events (
		transport, endpoint_name, status, latency_ms, started_at, ended_at
	) VALUES ('rest', 'invalid', 'failed', 1, NOW(), NOW())`); err == nil {
		t.Fatal("v9 accepted a new REST receipt without exact app identity")
	}
}

// assertRESTExecutionMigrationLedger verifies v1-v8 identities and timestamps
// are preserved while v9 is appended once with its frozen identity.
func assertRESTExecutionMigrationLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_engine_schema_migrations`).Scan(&count); err != nil || count != len(engineMigrations()) {
		t.Fatalf("REST migration ledger count=%d err=%v", count, err)
	}
	seededAt := restExecutionMigrationSeedTime()
	for _, migration := range engineMigrations()[:8] {
		assertManagedOAuthRefreshPriorMigration(t, ctx, pool, migration, seededAt)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM fused_engine_schema_migrations WHERE version = $1`, restExecutionMigrationVersion).Scan(&name); err != nil {
		t.Fatalf("read REST migration identity: %v", err)
	}
	if name != restExecutionMigrationName {
		t.Fatalf("REST migration name=%q, want %q", name, restExecutionMigrationName)
	}
}

// restExecutionMigrationSeedTime returns one stable timestamp shared by the
// fixture and immutable-ledger assertions.
func restExecutionMigrationSeedTime() time.Time {
	return time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
}
