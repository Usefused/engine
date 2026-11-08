package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestMigratedUnifiedAppSource embeds saved metadata once and preserves code bytes after the declaration.
func TestMigratedUnifiedAppSource(t *testing.T) {
	legacy := []byte(`{"apiVersion":"fused/v1","kind":"unified_app","type":"unified_app","name":"customer","version":"1.0.0","bucket":"default","services":{"crm":{"version":"v1","operations":["createCustomer"]}},"source":"export default buildUnifiedApp({});"}`)
	converted, status := migratedUnifiedAppSource(legacy)
	if status != unifiedSourceMigrated || !strings.HasPrefix(converted, "export const fusedConfig = {") || !strings.HasSuffix(converted, "export default buildUnifiedApp({});") {
		t.Fatalf("legacy source conversion status = %v", status)
	}
	// The metadata must carry the exact original service selection without nested source transport.
	if !strings.Contains(converted, `"createCustomer"`) || strings.Contains(converted[:strings.Index(converted, ";\n\n")], `"source"`) || strings.Contains(converted[:strings.Index(converted, ";\n\n")], `"type"`) {
		t.Fatal("migrated metadata changed the selected app scope")
	}
	already := []byte(`{"kind":"unified_app","name":"customer","source":"export const fusedConfig = {\"name\":\"customer\"};\nexport default buildUnifiedApp({});"}`)
	if _, status := migratedUnifiedAppSource(already); status != unifiedSourceAlreadyOwned {
		t.Fatalf("source-owned app status = %v", status)
	}
	commented := []byte(`{"kind":"unified_app","name":"customer","source":"/*\nexport const fusedConfig = {\"name\":\"example\"};\n*/\nexport default buildUnifiedApp({});"}`)
	// A sample declaration in a comment must not leave legacy source split from its config.
	if converted, status := migratedUnifiedAppSource(commented); status != unifiedSourceMigrated || !strings.HasPrefix(converted, "export const fusedConfig =") {
		t.Fatalf("commented declaration status = %v", status)
	}
	quoted := []byte("{\"kind\":\"unified_app\",\"source\":\"const example = `\\nexport const fusedConfig = {\\\"name\\\":\\\"example\\\"};\\n`;\\nexport default buildUnifiedApp({});\"}")
	// A declaration-shaped template string is only documentation, so the real config still needs embedding.
	if _, status := migratedUnifiedAppSource(quoted); status != unifiedSourceMigrated {
		t.Fatalf("quoted declaration status = %v", status)
	}
	crlf := []byte(`{"kind":"unified_app","source":"import { buildUnifiedApp } from '@fused/sdk';\r\nexport default buildUnifiedApp({});\r\n"}`)
	convertedCRLF, status := migratedUnifiedAppSource(crlf)
	// Migration adds a declaration while preserving the original code bytes and line endings.
	if status != unifiedSourceMigrated || !strings.HasSuffix(convertedCRLF, "import { buildUnifiedApp } from '@fused/sdk';\r\nexport default buildUnifiedApp({});\r\n") {
		t.Fatalf("line ending conversion status = %v", status)
	}
	// An oversized conversion cannot create a TypeScript file rejected by the CLI.
	large, err := json.Marshal(map[string]string{"kind": "unified_app", "source": strings.Repeat("x", unifiedSourceMigrationMaxBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if _, status := migratedUnifiedAppSource(large); status != unifiedSourceSkip {
		t.Fatalf("oversized declaration status = %v", status)
	}
	for _, source := range [][]byte{[]byte(`{"kind":"unified_app","source":""}`), []byte(`{"kind":"unified_app","bundle_digest":"sha256:abc"}`), []byte(`{"kind":"sdk","source":"export default buildUnifiedApp({});"}`), []byte(`[]`)} {
		// Missing or malformed source must remain untouched during startup.
		if _, status := migratedUnifiedAppSource(source); status != unifiedSourceSkip {
			t.Fatalf("unavailable source status = %v", status)
		}
	}
}

// TestMigrateUnifiedAppSourcesPersistsOnlyAuthoring exercises startup persistence on an isolated PostgreSQL database.
func TestMigrateUnifiedAppSourcesPersistsOnlyAuthoring(t *testing.T) {
	dsn := os.Getenv("FUSED_TEST_DATABASE_URL")
	// A dedicated database is required because schema initialization and migration write durable rows.
	if dsn == "" {
		t.Skip("FUSED_TEST_DATABASE_URL is required for the startup source migration integration test")
	}
	ctx := context.Background()
	pool, err := db.InitEnginePostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	subjectID, configID := uuid.New(), uuid.New()
	configKey := "unified_app:migration-" + configID.String() + ":1.0.0"
	if _, err := pool.Exec(ctx, `INSERT INTO fused_subjects(id,kind,display_name) VALUES($1,'user','Source Migration Test')`, subjectID); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM fused_subjects WHERE id=$1`, subjectID)
	defer pool.Exec(ctx, `DELETE FROM fused_config_states WHERE id=$1`, configID)
	before := []byte(`{"apiVersion":"fused/v1","kind":"unified_app","name":"migration-test","version":"1.0.0","bucket":"default","services":{"crm":{"version":"v1","operations":["createCustomer"]}},"source":"export default buildUnifiedApp({});"}`)
	if _, err := pool.Exec(ctx, `INSERT INTO fused_config_states(id,config_key,config_type,owner_subject_id,source_hash,desired_state) VALUES($1,$2,'unified_app',$3,'sha256:original',$4)`, configID, configKey, subjectID, before); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore)
	first, err := store.MigrateUnifiedAppSources(ctx)
	if err != nil || first.MigratedRows < 1 {
		t.Fatalf("first startup migration = %+v, %v", first, err)
	}
	var desired []byte
	var hash string
	var marker bool
	if err := pool.QueryRow(ctx, `SELECT desired_state,source_hash,unified_source_migrated_at IS NOT NULL FROM fused_config_states WHERE id=$1`, configID).Scan(&desired, &hash, &marker); err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(desired, &document); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := json.Unmarshal(document["source"], &source); err != nil {
		t.Fatal(err)
	}
	// Immutable receipt identity stays fixed while the authorized editor receives one-file source.
	if hash != "sha256:original" || !marker || !strings.Contains(source, "export const fusedConfig =") || !strings.HasSuffix(source, "export default buildUnifiedApp({});") {
		t.Fatalf("migrated state hash=%q marker=%v source-owned=%v", hash, marker, strings.Contains(source, "fusedConfig"))
	}
	state, err := NewPostgresConfigRepository(pool).GetConfigState(ctx, configKey)
	// The authenticated editor's repository path must observe the migrated source without a redeploy.
	if err != nil || state == nil || !strings.Contains(string(state.DesiredState), "fusedConfig") || state.SourceHash != "sha256:original" {
		t.Fatalf("editor source read did not retain migrated authoring and original hash: %v", err)
	}
	again, err := store.MigrateUnifiedAppSources(ctx)
	if err != nil || again.MigratedRows != 0 || again.AlreadyOwned != 0 {
		t.Fatalf("repeat startup migration = %+v, %v", again, err)
	}
}
