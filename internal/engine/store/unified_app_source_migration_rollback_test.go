package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestMigrateUnifiedAppSourcesRollsBackBatch verifies that a failed row cannot leave earlier rows half-migrated.
func TestMigrateUnifiedAppSourcesRollsBackBatch(t *testing.T) {
	dsn := os.Getenv("FUSED_TEST_DATABASE_URL")
	// Rollback needs a disposable database because the test adds a temporary table constraint.
	if dsn == "" {
		t.Skip("FUSED_TEST_DATABASE_URL is required for the startup source migration integration test")
	}
	ctx := context.Background()
	pool, err := db.InitEnginePostgres(ctx, dsn)
	// The migration is tested against the same schema initialized on Engine start.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	subjectID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_subjects(id,kind,display_name) VALUES($1,'user','Source Migration Rollback')`, subjectID); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM fused_subjects WHERE id=$1`, subjectID)
	defer pool.Exec(ctx, `DELETE FROM fused_config_states WHERE owner_subject_id=$1`, subjectID)
	firstID := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	failingID := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	const oldSource = "export default buildUnifiedApp({});"
	for _, id := range []uuid.UUID{firstID, failingID} {
		// Ordered UUIDs make the first update succeed before the injected second-row failure.
		if _, err := pool.Exec(ctx, `INSERT INTO fused_config_states(id,config_key,config_type,owner_subject_id,source_hash,desired_state) VALUES($1,$2,'unified_app',$3,'sha256:unchanged',jsonb_build_object('kind','unified_app','name','migration-rollback','version','1.0.0','source',$4::text))`, id, "unified_app:migration-rollback-"+id.String()+":1.0.0", subjectID, oldSource); err != nil {
			t.Fatal(err)
		}
	}
	constraint := fmt.Sprintf(`ALTER TABLE fused_config_states ADD CONSTRAINT fused_test_source_migration_rollback CHECK (id <> '%s'::uuid OR unified_source_migrated_at IS NULL)`, failingID)
	// The constraint rejects only migration of the second fixture after the first has been updated.
	if _, err := pool.Exec(ctx, constraint); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `ALTER TABLE fused_config_states DROP CONSTRAINT IF EXISTS fused_test_source_migration_rollback`)
	migrator := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore)
	result, err := migrator.MigrateUnifiedAppSources(ctx)
	// A failed transaction must report no committed rows even if it updated one before failing.
	if err == nil || result.MigratedRows != 0 {
		t.Fatalf("failed migration result = %+v, %v", result, err)
	}
	for _, id := range []uuid.UUID{firstID, failingID} {
		var source, hash string
		var marked bool
		// Both rows must retain their old source and immutable hash after rollback.
		if err := pool.QueryRow(ctx, `SELECT desired_state->>'source',source_hash,unified_source_migrated_at IS NOT NULL FROM fused_config_states WHERE id=$1`, id).Scan(&source, &hash, &marked); err != nil {
			t.Fatal(err)
		}
		if source != oldSource || hash != "sha256:unchanged" || marked {
			t.Fatalf("failed migration persisted partial row %s", id)
		}
	}
	// Removing the injected failure allows an ordinary restart to finish the page.
	if _, err := pool.Exec(ctx, `ALTER TABLE fused_config_states DROP CONSTRAINT fused_test_source_migration_rollback`); err != nil {
		t.Fatal(err)
	}
	retried, err := migrator.MigrateUnifiedAppSources(ctx)
	if err != nil || retried.MigratedRows != 2 {
		t.Fatalf("retry migration result = %+v, %v", retried, err)
	}
	for _, id := range []uuid.UUID{firstID, failingID} {
		var source string
		// A successful retry must migrate both rows once rather than duplicating a declaration.
		if err := pool.QueryRow(ctx, `SELECT desired_state->>'source' FROM fused_config_states WHERE id=$1`, id).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if strings.Count(source, "export const fusedConfig =") != 1 {
			t.Fatalf("retry produced duplicate or missing declaration for %s", id)
		}
	}
}
