package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type sourceMigrationFixture struct {
	id           uuid.UUID
	kind, source string
	desired      []byte
	oldMarker    bool
	status       unifiedSourceMigrationStatus
}

// sourceMigrationTestDatabase uses an explicitly isolated database for durable startup migration tests.
func sourceMigrationTestDatabase(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("FUSED_TEST_DATABASE_URL")
	// Durable migration tests must never discover or fall back to an operator's configured Engine database.
	if dsn == "" {
		t.Skip("FUSED_TEST_DATABASE_URL is required for source migration integration tests")
	}
	ctx := context.Background()
	pool, err := db.InitEnginePostgres(ctx, dsn)
	// The real startup schema is part of the migration contract under test.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// sourceMigrationTestOwner scopes every fixture and cleanup to one disposable test owner.
func sourceMigrationTestOwner(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	subject := uuid.New()
	sourceMigrationExec(t, ctx, pool, `INSERT INTO fused_subjects(id,kind,display_name) VALUES($1,'user','Source Format Migration Test')`, subject)
	t.Cleanup(func() {
		// Only this fixture owner's rows are disposable; no unrelated app state participates in cleanup.
		_, _ = pool.Exec(ctx, `DELETE FROM fused_config_states WHERE owner_subject_id=$1`, subject)
		_, _ = pool.Exec(ctx, `DELETE FROM fused_subjects WHERE id=$1`, subject)
	})
	return subject
}

// sourceMigrationExec makes setup and fault injection fail before a misleading migration assertion can run.
func sourceMigrationExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	// Every setup statement must commit successfully to exercise its intended migration path.
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// insertSourceMigrationFixture models both unmarked YAML source and an export marked by the original migration.
func insertSourceMigrationFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, owner, id uuid.UUID, kind, source string, oldMarker bool, status unifiedSourceMigrationStatus) sourceMigrationFixture {
	t.Helper()
	desired := migrationSourceFixture(t, kind, source)
	sourceMigrationExec(t, ctx, pool, `INSERT INTO fused_config_states(id,config_key,config_type,owner_subject_id,source_hash,desired_state,unified_source_migrated_at) VALUES($1,$2,$3,$4,'sha256:unchanged',$5,CASE WHEN $6 THEN '2000-01-01T00:00:00Z'::timestamptz ELSE NULL END)`, id, kind+":"+id.String()+":1.0.0", kind, owner, desired, oldMarker)
	return sourceMigrationFixture{id: id, kind: kind, source: source, desired: desired, oldMarker: oldMarker, status: status}
}

// assertSourceMigrationFixture checks authoring-only changes while preserving every immutable identity and receipt field.
func assertSourceMigrationFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture sourceMigrationFixture) {
	t.Helper()
	var desired []byte
	var hash string
	var generation, format int
	var marked, oldMarker bool
	// Reading the durable row proves the marker and source committed atomically rather than only changing aggregate counts.
	if err := pool.QueryRow(ctx, `SELECT desired_state,source_hash,generation,unified_source_format_version,unified_source_migrated_at IS NOT NULL,COALESCE(unified_source_migrated_at='2000-01-01T00:00:00Z',false) FROM fused_config_states WHERE id=$1`, fixture.id).Scan(&desired, &hash, &generation, &format, &marked, &oldMarker); err != nil {
		t.Fatal(err)
	}
	// Runtime identity, generation and the original source receipt remain outside this authoring migration.
	if hash != "sha256:unchanged" || generation != 1 {
		t.Fatal("migration changed immutable state")
	}
	var current map[string]json.RawMessage
	// Desired state must remain a valid JSON envelope after source replacement.
	if err := json.Unmarshal(desired, &current); err != nil {
		t.Fatal(err)
	}
	var source string
	// The authenticated source read uses this same stored string.
	if err := json.Unmarshal(current["source"], &source); err != nil {
		t.Fatal(err)
	}
	assertSourceMigrationOutcome(t, fixture, source, format, marked, oldMarker)
	delete(current, "source")
	var original map[string]json.RawMessage
	_ = json.Unmarshal(fixture.desired, &original)
	delete(original, "source")
	before := migrationCanonicalJSON(t, original)
	after := migrationCanonicalJSON(t, current)
	// No deployment setting can change merely because its source representation changed.
	if string(before) != string(after) {
		t.Fatal("migration changed persisted configuration outside source")
	}
}

// assertSourceMigrationOutcome keeps unsupported rows untouched and admits only canonical config for completed rows.
func assertSourceMigrationOutcome(t *testing.T, fixture sourceMigrationFixture, source string, format int, marked, oldMarker bool) {
	t.Helper()
	// Missing source and unrelated app kinds are not authoring formats this migration can convert.
	if fixture.status == unifiedSourceSkip {
		if source != fixture.source || format != 0 || marked != fixture.oldMarker {
			t.Fatal("unsupported fixture was modified")
		}
		return
	}
	assertSourceMigrationMarker(t, fixture, format, marked, oldMarker)
	// Current source is marked without any byte changes or recompilation.
	if fixture.status == unifiedSourceAlreadyOwned {
		if source != fixture.source {
			t.Fatal("current source was rewritten")
		}
		return
	}
	assertCurrentMigrationSource(t, source, fixture.desired)
}

// migrationCanonicalJSON compares JSON values rather than PostgreSQL jsonb or encoder whitespace.
func migrationCanonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	// A malformed assertion value cannot prove migration fidelity.
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	// Decode before comparing so nested RawMessages receive the same semantic normalization as jsonb.
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(decoded)
	// Normalized JSON must remain serializable after the round trip.
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// assertSourceMigrationMarker verifies the independent format version while retaining the old migration's historical timestamp.
func assertSourceMigrationMarker(t *testing.T, fixture sourceMigrationFixture, format int, marked, oldMarker bool) {
	t.Helper()
	// The new version marker must be independent of the earlier migration timestamp and must retain its historical value.
	if format != unifiedSourceFormatVersion || !marked || (fixture.oldMarker && !oldMarker) {
		t.Fatal("format-v2 progress was not recorded correctly")
	}
}
