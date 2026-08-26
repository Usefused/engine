package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestActivityReceiptMigrationPreservesHistoricalRows verifies forward upgrade rather than fresh-schema coincidence.
func TestActivityReceiptMigrationPreservesHistoricalRows(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Integration checks require an explicit disposable database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedEngineMigrationPool(t, ctx, databaseURL)
	seedActivityReceiptV12(t, ctx, pool)
	id := uuid.New()
	// A supported pre-upgrade receipt has no hierarchy metadata to backfill.
	if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_execution_events (id,transport,direction,endpoint_name,status,latency_ms,started_at,ended_at) VALUES ($1,'webhook','inbound','items.changed','success',1,NOW(),NOW())`, id); err != nil {
		t.Fatal(err)
	}
	// Normal startup owns the forward migration under its advisory lock.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var kind string
	var parent *uuid.UUID
	var steps int
	// Inspect the unchanged historical row after the new columns are installed.
	if err := pool.QueryRow(ctx, `SELECT execution_kind,parent_execution_id,jsonb_array_length(unified_steps) FROM fused_engine_execution_events WHERE id=$1`, id).Scan(&kind, &parent, &steps); err != nil {
		t.Fatal(err)
	}
	// Old receipts remain ordinary provider history, never fabricated parents.
	if kind != "physical" || parent != nil || steps != 0 {
		t.Fatal("historical receipt semantics changed")
	}
	assertActivityReceiptMigrationRestart(t, ctx, pool)
}

// assertActivityReceiptMigrationRestart proves ordinary startup is idempotent after the additive upgrade.
func assertActivityReceiptMigrationRestart(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// A second startup must skip the already-recorded migration.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var count int
	// The ledger is authoritative rather than checking for columns alone.
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_engine_schema_migrations WHERE version=13`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// Restart must not create another migration ledger row.
	if count != 1 {
		t.Fatal("activity migration was not applied exactly once")
	}
}

// seedActivityReceiptV12 applies the frozen previous ledger to an owned schema.
func seedActivityReceiptV12(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range engineSchemaQueries() {
		// Canonical tables establish the previous supported shape before additive upgrade.
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	for _, migration := range engineMigrations()[:12] {
		// Preserve every earlier ledger identity so normal startup executes only migration thirteen.
		if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name); err != nil {
			t.Fatal(err)
		}
	}
}
