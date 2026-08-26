package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMCPSessionLifetimeMigrationOwnsEndReasonChange keeps fresh installs aligned without rewriting v10/v11.
func TestMCPSessionLifetimeMigrationOwnsEndReasonChange(t *testing.T) {
	fresh := engineSchemaTable(t, "fused_mcp_sessions")
	migration := strings.Join(mcpSessionLifetimeMigrationQueries(), "\n")
	assertSchemaContainsAll(t, fresh, "fresh MCP lifecycle constraint missing %q", []string{"'max_lifetime'", "'tool_call_timeout'"})
	assertSchemaContainsAll(t, migration, "MCP lifetime migration missing %q", []string{
		"DROP CONSTRAINT IF EXISTS chk_fused_mcp_sessions_end_reason",
		"ADD CONSTRAINT chk_fused_mcp_sessions_end_reason", "'max_lifetime'", "'tool_call_timeout'",
	})
	frozen := strings.Join(append(appTokenHistoryMigrationQueries(), appTokenCleanupMigrationQueries()...), "\n")
	// Recorded migrations must not acquire new policy that previously upgraded installations would skip.
	if strings.Contains(frozen, "max_lifetime") {
		t.Fatal("MCP lifetime support rewrote immutable migrations v10/v11")
	}
}

// TestMCPSessionLifetimeMigrationUpgradesV11Sessions proves hard stops persist after a real forward upgrade.
func TestMCPSessionLifetimeMigrationUpgradesV11Sessions(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Integration tests use an explicitly selected test database and never invent a live fallback.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedEngineMigrationPool(t, ctx, databaseURL)
	seedMCPSessionLifetimeV11Schema(t, ctx, pool)
	legacyID := insertMCPLifetimeSession(t, ctx, pool, "tool_call_timeout")
	assertMCPLifetimeReasonRejected(t, ctx, pool, "max_lifetime")
	// Normal Engine startup must append v12 before the worker writes the new producer cause.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("initialize MCP lifetime migration: %v", err)
	}
	assertMCPLifetimeSession(t, ctx, pool, legacyID, "tool_call_timeout")
	hardStopID := insertMCPLifetimeSession(t, ctx, pool, "max_lifetime")
	assertMCPLifetimeSession(t, ctx, pool, hardStopID, "max_lifetime")
	assertMCPLifetimeReasonRejected(t, ctx, pool, "unrecognized_reason")
	appliedAt := readMCPLifetimeMigrationTime(t, ctx, pool)
	// A second startup must preserve the ledger timestamp and existing session evidence.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("reinitialize MCP lifetime migration: %v", err)
	}
	assertMCPLifetimeMigrationLedger(t, ctx, pool, appliedAt)
	assertMCPLifetimeSession(t, ctx, pool, hardStopID, "max_lifetime")
}

// seedMCPSessionLifetimeV11Schema restores the frozen previous CHECK while recording prior migration identities.
func seedMCPSessionLifetimeV11Schema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range engineSchemaQueries() {
		// Canonical tables isolate the constraint upgrade from unrelated legacy shape repair.
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("create MCP lifetime fixture schema: %v", err)
		}
	}
	for _, migration := range engineMigrations()[:11] {
		// Stable prior timestamps make accidental immutable-ledger rewrites observable.
		if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`, migration.Version, migration.Name, mcpLifetimeFixtureTime()); err != nil {
			t.Fatalf("seed MCP lifetime prior migration %d: %v", migration.Version, err)
		}
	}
	legacyConstraint := schemaQueryContaining(appTokenCleanupMigrationQueries(), "ADD CONSTRAINT chk_fused_mcp_sessions_end_reason")
	for _, query := range []string{`ALTER TABLE fused_mcp_sessions DROP CONSTRAINT chk_fused_mcp_sessions_end_reason`, legacyConstraint} {
		// Reusing frozen v11 DDL avoids a test-only approximation of the production upgrade boundary.
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("restore MCP lifetime v11 constraint: %v", err)
		}
	}
}

// insertMCPLifetimeSession creates one compact termination row whose timestamps must survive schema changes.
func insertMCPLifetimeSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, reason string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_mcp_sessions (id, session_id, end_reason, ended_at, last_activity_at) VALUES ($1, $2, $3, $4, $4)`, id, id.String(), reason, mcpLifetimeFixtureTime())
	// Each admitted cause must be writable through the actual PostgreSQL CHECK, not only a Go allowlist.
	if err != nil {
		t.Fatalf("insert MCP lifetime session: %v", err)
	}
	return id
}

// assertMCPLifetimeSession checks exact durable cause and time preservation after upgrade and replay.
func assertMCPLifetimeSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, reason string) {
	t.Helper()
	var actualReason string
	var endedAt, lastActivityAt time.Time
	err := pool.QueryRow(ctx, `SELECT end_reason, ended_at, last_activity_at FROM fused_mcp_sessions WHERE id = $1`, id).Scan(&actualReason, &endedAt, &lastActivityAt)
	// Missing or unreadable rows are audit evidence loss, not an acceptable migration side effect.
	if err != nil {
		t.Fatalf("read MCP lifetime session: %v", err)
	}
	// A constraint-only change must never normalize historical reasons or alter producer timestamps.
	if actualReason != reason || !endedAt.Equal(mcpLifetimeFixtureTime()) || !lastActivityAt.Equal(mcpLifetimeFixtureTime()) {
		t.Fatalf("MCP lifetime session = %q/%s/%s, want %q and original timestamps", actualReason, endedAt, lastActivityAt, reason)
	}
}

// assertMCPLifetimeReasonRejected proves the old/new allowlist boundaries remain enforced by PostgreSQL.
func assertMCPLifetimeReasonRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, reason string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO fused_mcp_sessions (id, end_reason) VALUES ($1, $2)`, uuid.New(), reason)
	var constraintError *pgconn.PgError
	// Only the expected named CHECK violation proves the lifecycle vocabulary rejected the input.
	if !errors.As(err, &constraintError) || constraintError.Code != "23514" || constraintError.ConstraintName != "chk_fused_mcp_sessions_end_reason" {
		t.Fatalf("MCP lifetime reason %q rejection = %v, want lifecycle CHECK violation", reason, err)
	}
}

// readMCPLifetimeMigrationTime validates v12's frozen identity while capturing its first successful application.
func readMCPLifetimeMigrationTime(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var name string
	var appliedAt time.Time
	err := pool.QueryRow(ctx, `SELECT name, applied_at FROM fused_engine_schema_migrations WHERE version = $1`, mcpSessionLifetimeMigrationVersion).Scan(&name, &appliedAt)
	// The recorded version must identify the exact append-only migration, not merely any v12 row.
	if err != nil || name != mcpSessionLifetimeMigrationName {
		t.Fatalf("MCP lifetime migration identity = %q/%v", name, err)
	}
	return appliedAt
}

// assertMCPLifetimeMigrationLedger proves both new and historical ledger entries remain unchanged on restart.
func assertMCPLifetimeMigrationLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, appliedAt time.Time) {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_engine_schema_migrations`).Scan(&count)
	// A repeated startup must not append duplicate or unregistered migration records.
	if err != nil || count != len(engineMigrations()) {
		t.Fatalf("MCP lifetime migration ledger count = %d/%v", count, err)
	}
	// Replaying a completed migration would change its timestamp even if the DDL is idempotent.
	if !readMCPLifetimeMigrationTime(t, ctx, pool).Equal(appliedAt) {
		t.Fatal("MCP lifetime migration was replayed after it was recorded")
	}
	for _, migration := range engineMigrations()[:11] {
		assertManagedOAuthRefreshPriorMigration(t, ctx, pool, migration, mcpLifetimeFixtureTime())
	}
}

// mcpLifetimeFixtureTime provides one deterministic producer and prior-ledger timestamp.
func mcpLifetimeFixtureTime() time.Time {
	return time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
}
