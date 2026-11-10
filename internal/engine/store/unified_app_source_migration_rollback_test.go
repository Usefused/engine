package store

import (
	"fmt"
	"github.com/google/uuid"
	"testing"
)

// TestMigrateUnifiedAppSourcesRollsBackBatch protects source and format markers from partial page writes or false success counts.
func TestMigrateUnifiedAppSourcesRollsBackBatch(t *testing.T) {
	ctx, pool := sourceMigrationTestDatabase(t)
	owner := sourceMigrationTestOwner(t, ctx, pool)
	firstID := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	failingID := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	const oldSource = "export const fusedConfig = {};\nexport default buildUnifiedApp({});"
	fixtures := []sourceMigrationFixture{
		insertSourceMigrationFixture(t, ctx, pool, owner, firstID, "unified_app", oldSource, true, unifiedSourceSkip),
		insertSourceMigrationFixture(t, ctx, pool, owner, failingID, "unified_app", oldSource, true, unifiedSourceSkip),
	}
	constraint := fmt.Sprintf(`ALTER TABLE fused_config_states ADD CONSTRAINT fused_test_source_migration_rollback CHECK (id <> '%s'::uuid OR unified_source_format_version < 2)`, failingID)
	sourceMigrationExec(t, ctx, pool, constraint)
	t.Cleanup(func() {
		// Fault injection is scoped to this test and cannot survive cleanup into another migration run.
		_, _ = pool.Exec(ctx, `ALTER TABLE fused_config_states DROP CONSTRAINT IF EXISTS fused_test_source_migration_rollback`)
	})
	migrator := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore)
	result, err := migrator.MigrateUnifiedAppSources(ctx)
	// A second-row failure must roll back both updates and leave aggregate committed counts at zero.
	if err == nil || result.MigratedRows != 0 {
		t.Fatalf("failed migration=%+v %v", result, err)
	}
	// Both rows still have their exact old source, timestamp, hash and pre-v2 format marker.
	for _, fixture := range fixtures {
		assertSourceMigrationFixture(t, ctx, pool, fixture)
	}
	sourceMigrationExec(t, ctx, pool, `ALTER TABLE fused_config_states DROP CONSTRAINT fused_test_source_migration_rollback`)
	retried, err := migrator.MigrateUnifiedAppSources(ctx)
	// Removing the fault must allow an ordinary startup retry to commit this same page exactly once.
	if err != nil || retried.MigratedRows != 2 {
		t.Fatalf("retry migration=%+v %v", retried, err)
	}
	for _, fixture := range fixtures {
		// The retry's expected outcome changes only after the failing transaction has been proven atomic.
		fixture.status = unifiedSourceMigrated
		assertSourceMigrationFixture(t, ctx, pool, fixture)
	}
}
