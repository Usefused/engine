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

// TestGenerationContractPinSchemaPreservesOldSnapshots checks fresh and upgraded schemas share the same additive storage boundary.
func TestGenerationContractPinSchemaPreservesOldSnapshots(t *testing.T) {
	for _, schema := range []string{strings.Join(engineSchemaQueries(), "\n"), strings.Join(generationContractPinMigrationQueries(), "\n")} {
		// The empty default deliberately leaves legacy runtime snapshots unpinned until an explicit refresh.
		if !strings.Contains(schema, "generation_contract_hash text NOT NULL DEFAULT ''") || !strings.Contains(schema, "^sha256:[0-9a-f]{64}$") {
			t.Fatal("missing canonical generation pin column")
		}
	}
	migrations := engineMigrations()
	pinMigration := migrations[13]
	// New pin storage must not rewrite already-applied migration identities.
	if pinMigration.Version != 14 || pinMigration.Name != "20260826_generation_contract_pins" {
		t.Fatalf("migration=%+v", pinMigration)
	}
}

// TestGenerationPinMigrationPreservesLegacySnapshots proves startup upgrades v13 without inventing archive identities or replaying the new migration.
func TestGenerationPinMigrationPreservesLegacySnapshots(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// Migration tests require an explicit isolated target rather than a default database.
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedEngineMigrationPool(t, ctx, dsn)
	versionID := seedGenerationPinV13Snapshot(t, ctx, pool)
	// The canonical startup path must append the pin column on an existing installation.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	first := assertGenerationPinMigrationState(t, ctx, pool, versionID)
	// A repeated startup must preserve data and the immutable migration timestamp.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	second := assertGenerationPinMigrationState(t, ctx, pool, versionID)
	// Pin support must remain a one-shot migration, not recurring schema work.
	if !first.Equal(second) {
		t.Fatal("generation pin migration replayed")
	}
}

// seedGenerationPinV13Snapshot restores the exact pre-pin shape inside a UUID-owned schema.
func seedGenerationPinV13Snapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	for _, query := range engineSchemaQueries() {
		// Reuse canonical DDL so the fixture differs only in the new feature's column.
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	for _, migration := range engineMigrations()[:13] {
		// Recorded prior identities ensure startup tests only the appended upgrade.
		if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name); err != nil {
			t.Fatal(err)
		}
	}
	// This destructive DDL targets only the disposable test schema, never the selected database's public tables.
	if _, err := pool.Exec(ctx, `ALTER TABLE fused_service_contract_snapshots DROP COLUMN generation_contract_hash`); err != nil {
		t.Fatal(err)
	}
	versionID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_service_contract_snapshots(service_id,service_version_id,version,contract_version,required_capabilities,revision,source_hash,contract_hash,service_metadata)
	 VALUES($1,$2,'v1',2,'{}',7,'original-source','original-runtime','{}')`, uuid.New(), versionID)
	// Legacy snapshots contain no trustworthy generation hash for startup to infer.
	if err != nil {
		t.Fatal(err)
	}
	return versionID
}

// assertGenerationPinMigrationState verifies legacy data remains intact and explicitly unpinned.
func assertGenerationPinMigrationState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionID uuid.UUID) time.Time {
	t.Helper()
	var hash, source string
	var revision int
	// A missing or rewritten legacy snapshot would break already-running integrations.
	if err := pool.QueryRow(ctx, `SELECT generation_contract_hash,source_hash,revision FROM fused_service_contract_snapshots WHERE service_version_id=$1`, versionID).Scan(&hash, &source, &revision); err != nil {
		t.Fatal(err)
	}
	// Retention authority cannot be backfilled by guessing from an unrelated runtime hash.
	if hash != "" || source != "original-source" || revision != 7 {
		t.Fatalf("legacy state hash=%q source=%q revision=%d", hash, source, revision)
	}
	var appliedAt time.Time
	// The migration ledger supplies the restart idempotency evidence.
	if err := pool.QueryRow(ctx, `SELECT applied_at FROM fused_engine_schema_migrations WHERE version=14 AND name='20260826_generation_contract_pins'`).Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	return appliedAt
}
