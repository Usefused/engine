package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestMigrateUnifiedAppSourcesMatrix exercises mixed records, page boundaries, and simultaneous Engine starts.
func TestMigrateUnifiedAppSourcesMatrix(t *testing.T) {
	dsn := os.Getenv("FUSED_TEST_DATABASE_URL")
	// A dedicated database keeps this durable migration matrix separate from real app states.
	if dsn == "" {
		t.Skip("FUSED_TEST_DATABASE_URL is required for the startup source migration integration test")
	}
	ctx := context.Background()
	pool, err := db.InitEnginePostgres(ctx, dsn)
	// Schema initialization is part of the startup path exercised by this test.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	subjectID := uuid.New()
	// Every test row has one owner so cleanup cannot touch another fixture.
	if _, err := pool.Exec(ctx, `INSERT INTO fused_subjects(id,kind,display_name) VALUES($1,'user','Source Migration Matrix')`, subjectID); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM fused_subjects WHERE id=$1`, subjectID)
	defer pool.Exec(ctx, `DELETE FROM fused_config_states WHERE owner_subject_id=$1`, subjectID)
	const legacyCount = unifiedSourceMigrationBatchSize + 5
	legacyIDs := make([]uuid.UUID, 0, legacyCount)
	// More than one page is required to verify the UUID cursor and transaction boundary.
	for index := 0; index < legacyCount; index++ {
		id := uuid.New()
		legacyIDs = append(legacyIDs, id)
		name := fmt.Sprintf("migration-matrix-%03d", index)
		source := "export default buildUnifiedApp({});"
		// A comment-shaped declaration in the first page must still receive real metadata.
		if index == 0 {
			source = "/*\nexport const fusedConfig = {\"name\":\"example\"};\n*/\n" + source
		}
		desired, err := json.Marshal(map[string]any{
			"apiVersion": "fused/v1", "kind": "unified_app", "name": name, "version": "1.0.0",
			"bucket": "default", "services": map[string]any{"crm": map[string]any{"version": "v1", "operations": []string{"createCustomer"}}}, "source": source,
		})
		// Serialization failure must not insert a partial fixture.
		if err != nil {
			t.Fatal(err)
		}
		// Unique config keys model distinct immutable app versions across two migration pages.
		if _, err := pool.Exec(ctx, `INSERT INTO fused_config_states(id,config_key,config_type,owner_subject_id,source_hash,desired_state) VALUES($1,$2,'unified_app',$3,'sha256:unchanged',$4)`, id, "unified_app:"+name+":1.0.0", subjectID, desired); err != nil {
			t.Fatal(err)
		}
	}
	ownedID, missingID, sdkID := uuid.New(), uuid.New(), uuid.New()
	fixtures := []struct {
		id         uuid.UUID
		configType string
		name       string
		source     string
	}{
		{ownedID, "unified_app", "migration-matrix-owned", "export const fusedConfig = {\"kind\":\"unified_app\"};\nexport default buildUnifiedApp({});"},
		{missingID, "unified_app", "migration-matrix-missing", ""},
		{sdkID, "sdk", "migration-matrix-sdk", "export default buildUnifiedApp({});"},
	}
	// Mixed records prove that the query selects only old Unified App authoring.
	for _, fixture := range fixtures {
		desired, err := json.Marshal(map[string]any{"kind": fixture.configType, "name": fixture.name, "version": "1.0.0", "source": fixture.source})
		// Fixture serialization must preserve the exact old or already-owned source.
		if err != nil {
			t.Fatal(err)
		}
		// Failure to save any control row would make the migration counts misleading.
		if _, err := pool.Exec(ctx, `INSERT INTO fused_config_states(id,config_key,config_type,owner_subject_id,source_hash,desired_state) VALUES($1,$2,$3,$4,'sha256:unchanged',$5)`, fixture.id, fixture.configType+":"+fixture.name+":1.0.0", fixture.configType, subjectID, desired); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct {
		result UnifiedAppSourceMigrationResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	// Two stores imitate Engine replicas starting against the same persisted app set.
	for replica := 0; replica < 2; replica++ {
		go func() {
			<-start
			result, err := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore).MigrateUnifiedAppSources(ctx)
			results <- outcome{result: result, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	// Row locks must prevent both replicas from converting one immutable version twice.
	if first.err != nil || second.err != nil || first.result.MigratedRows+second.result.MigratedRows != legacyCount || first.result.AlreadyOwned+second.result.AlreadyOwned != 1 {
		t.Fatalf("concurrent migration results = %+v, %+v", first, second)
	}
	// Inspect every converted row rather than trusting aggregate migration counts.
	for index, id := range legacyIDs {
		var desired []byte
		var hash string
		var generation int
		var marked bool
		// A persisted version's identity and receipt fields must survive its editor-source upgrade.
		if err := pool.QueryRow(ctx, `SELECT desired_state,source_hash,generation,unified_source_migrated_at IS NOT NULL FROM fused_config_states WHERE id=$1`, id).Scan(&desired, &hash, &generation, &marked); err != nil {
			t.Fatal(err)
		}
		var document struct {
			Name     string `json:"name"`
			Source   string `json:"source"`
			Services map[string]struct {
				Operations []string `json:"operations"`
			} `json:"services"`
		}
		// The stored source must remain a valid JSON transport value after replacement.
		if err := json.Unmarshal(desired, &document); err != nil {
			t.Fatal(err)
		}
		// Source ownership must coexist with unchanged identity, generation, and operation scope.
		if !marked || hash != "sha256:unchanged" || generation != 1 || document.Name != fmt.Sprintf("migration-matrix-%03d", index) || !strings.HasPrefix(document.Source, "export const fusedConfig = {") || !strings.HasSuffix(document.Source, "export default buildUnifiedApp({});") || len(document.Services["crm"].Operations) != 1 || document.Services["crm"].Operations[0] != "createCustomer" {
			t.Fatalf("legacy row %d changed identity or capability scope", index)
		}
	}
	// Control rows establish that only legacy Unified App source was rewritten.
	for _, fixture := range fixtures {
		var source string
		var marked bool
		// Already-owned source is marked once; missing source and other app kinds remain untouched.
		if err := pool.QueryRow(ctx, `SELECT desired_state->>'source',unified_source_migrated_at IS NOT NULL FROM fused_config_states WHERE id=$1`, fixture.id).Scan(&source, &marked); err != nil {
			t.Fatal(err)
		}
		// Owned code is marked once; unsupported and unrelated code remains byte-for-byte intact.
		if source != fixture.source || marked != (fixture.id == ownedID) {
			t.Fatalf("nonlegacy row %s changed unexpectedly", fixture.name)
		}
	}
	again, err := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore).MigrateUnifiedAppSources(ctx)
	// The only remaining work after a restart is the intentionally unconvertible missing-source row.
	if err != nil || again.MigratedRows != 0 || again.AlreadyOwned != 0 || again.SkippedRows != 1 {
		t.Fatalf("repeat startup result = %+v, %v", again, err)
	}
}
