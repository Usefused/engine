package store

import (
	"github.com/google/uuid"
	"testing"
)

type sourceMigrationOutcome struct {
	result UnifiedAppSourceMigrationResult
	err    error
}

// assertConcurrentMigrationCounts proves row locks partition conversion work rather than reporting the same app twice.
func assertConcurrentMigrationCounts(t *testing.T, first, second sourceMigrationOutcome, expected int) {
	t.Helper()
	// Both replicas must complete and collectively convert each predecessor and mark canonical source exactly once.
	if first.err != nil || second.err != nil || first.result.MigratedRows+second.result.MigratedRows != expected || first.result.AlreadyOwned+second.result.AlreadyOwned != 1 {
		t.Fatalf("concurrent migration results=%+v %+v", first, second)
	}
}

// TestMigrateUnifiedAppSourcesMatrix covers already-marked exports across multiple pages and simultaneous Engine starts.
func TestMigrateUnifiedAppSourcesMatrix(t *testing.T) {
	ctx, pool := sourceMigrationTestDatabase(t)
	owner := sourceMigrationTestOwner(t, ctx, pool)
	const legacyCount = unifiedSourceMigrationBatchSize + 5
	fixtures := make([]sourceMigrationFixture, 0, legacyCount+3)
	// Alternate both predecessor formats so the old timestamp can never suppress a required format-v2 rewrite.
	for index := 0; index < legacyCount; index++ {
		source := "export default buildUnifiedApp({});"
		oldMarker := index%2 == 0
		// The first migration already marked these exports; the second migration must still convert them.
		if oldMarker {
			source = "export const fusedConfig = {\"name\":\"stale\"};\n" + source
		}
		fixtures = append(fixtures, insertSourceMigrationFixture(t, ctx, pool, owner, uuid.New(), "unified_app", source, oldMarker, unifiedSourceMigrated))
	}
	fixtures = append(fixtures,
		insertSourceMigrationFixture(t, ctx, pool, owner, uuid.New(), "unified_app", "export default buildUnifiedApp({config:{\"name\":\"current\"}});", true, unifiedSourceAlreadyOwned),
		insertSourceMigrationFixture(t, ctx, pool, owner, uuid.New(), "unified_app", "", false, unifiedSourceSkip),
		insertSourceMigrationFixture(t, ctx, pool, owner, uuid.New(), "sdk", "export default buildUnifiedApp({});", false, unifiedSourceSkip))
	start := make(chan struct{})
	results := make(chan sourceMigrationOutcome, 2)
	// Row locking must make simultaneous startup scans converge without converting the same source twice.
	for replica := 0; replica < 2; replica++ {
		go func() {
			// Release both stores together so they compete for the same first page.
			<-start
			result, err := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore).MigrateUnifiedAppSources(ctx)
			results <- sourceMigrationOutcome{result, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	assertConcurrentMigrationCounts(t, first, second, legacyCount)
	// Inspect persisted state for every converted and control row, including earlier timestamp markers.
	for _, fixture := range fixtures {
		assertSourceMigrationFixture(t, ctx, pool, fixture)
	}
	again, err := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore).MigrateUnifiedAppSources(ctx)
	// A restart must find no old export or duplicate conversion; only the intentionally missing source remains pending.
	if err != nil || again.MigratedRows != 0 || again.AlreadyOwned != 0 || again.SkippedRows != 1 {
		t.Fatalf("repeat startup=%+v %v", again, err)
	}
}
