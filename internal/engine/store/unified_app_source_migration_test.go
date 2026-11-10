package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// migrationSourceFixture supplies authoritative saved metadata independently of any stale source declaration.
func migrationSourceFixture(t *testing.T, kind, source string) []byte {
	t.Helper()
	desired, err := json.Marshal(map[string]any{"apiVersion": "fused/v1", "kind": kind, "type": "unified_app", "name": "customer", "version": "1.0.0", "bucket": "default", "runtime_buckets": []string{"default", "deployment"}, "services": map[string]any{"crm": map[string]any{"version": "v1", "operations": []string{"createCustomer"}}}, "source": source})
	// Test inputs must be serialized through the same JSON envelope as persisted app state.
	if err != nil {
		t.Fatal(err)
	}
	return desired
}

// assertCurrentMigrationSource verifies canonical metadata and unchanged implementation without depending on formatting.
func assertCurrentMigrationSource(t *testing.T, source string, desired []byte) {
	t.Helper()
	starts, err := inlineConfigDeclarations([]byte(source))
	// Exactly one inline config is the only supported authoring authority after conversion.
	if err != nil || len(starts) != 1 || hasRetiredUnifiedMetadata(source) {
		t.Fatalf("noncanonical migrated source: %v", err)
	}
	var actual map[string]json.RawMessage
	// Read JSON statically; migration verification must never execute stored tenant code.
	if err := json.NewDecoder(strings.NewReader(source[starts[0]:])).Decode(&actual); err != nil {
		t.Fatal(err)
	}
	expected, _, ok := unifiedSourceMigrationInput(desired)
	// The persisted desired state remains authoritative even when an old embedded export was stale.
	if !ok {
		t.Fatal("invalid migration fixture")
	}
	actualJSON := migrationCanonicalJSON(t, actual)
	expectedJSON := migrationCanonicalJSON(t, expected)
	// Source transport and the retired YAML discriminator are excluded by the shared migration input projection.
	if string(actualJSON) != string(expectedJSON) {
		t.Fatalf("migrated metadata changed persisted configuration")
	}
}

// TestMigratedUnifiedAppSource covers both predecessor formats and accepts only current inline config as already migrated.
func TestMigratedUnifiedAppSource(t *testing.T) {
	const implementation = `execute() { return { preserved: "braces } and quotes" }; }`
	const body = "export default buildUnifiedApp({" + implementation + "});"
	cases := []struct {
		name, kind, source string
		status             unifiedSourceMigrationStatus
	}{
		{"yaml source", "unified_app", body, unifiedSourceMigrated},
		{"retired export", "unified_app", "export const fusedConfig = {\"name\":\"stale\"};\n" + body, unifiedSourceMigrated},
		{"current config", "unified_app", "export default buildUnifiedApp({config:{\"name\":\"customer\"}});", unifiedSourceAlreadyOwned},
		{"two old authorities", "unified_app", "export const fusedConfig = {};\nexport default buildUnifiedApp({config:{\"name\":\"stale\"}," + implementation + "});", unifiedSourceMigrated},
		{"comment example", "unified_app", "/* export const fusedConfig = {}; */\n" + body, unifiedSourceMigrated},
		{"quoted example", "unified_app", "const example = `export const fusedConfig = {};`;\n" + body, unifiedSourceMigrated},
		{"CRLF", "unified_app", "import {buildUnifiedApp} from '@fused/unified-app';\r\n" + body + "\r\n", unifiedSourceMigrated},
		{"referenced old binding", "unified_app", "export const fusedConfig = {};\nexport default buildUnifiedApp({execute(){return fusedConfig;}});", unifiedSourceSkip},
		{"missing source", "unified_app", "", unifiedSourceSkip},
		{"unrelated kind", "sdk", body, unifiedSourceSkip},
		{"oversized source", "unified_app", strings.Repeat("x", unifiedSourceMigrationMaxBytes), unifiedSourceSkip},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			desired := migrationSourceFixture(t, test.kind, test.source)
			converted, status := migratedUnifiedAppSource(desired)
			// Unsupported source remains untouched instead of retaining a compatibility interpretation.
			if status != test.status {
				t.Fatalf("migration status=%v want=%v", status, test.status)
			}
			if status != unifiedSourceMigrated {
				return
			}
			assertCurrentMigrationSource(t, converted, desired)
			// Conversion removes metadata declarations only; the authored execute body must remain byte-for-byte intact.
			if !strings.Contains(converted, implementation) {
				t.Fatal("migration changed executable implementation")
			}
		})
	}
}

// TestMigrateUnifiedAppSourcesPersistsOnlyAuthoring revisits an old timestamp without changing runtime identity or receipt hashes.
func TestMigrateUnifiedAppSourcesPersistsOnlyAuthoring(t *testing.T) {
	ctx, pool := sourceMigrationTestDatabase(t)
	owner := sourceMigrationTestOwner(t, ctx, pool)
	fixture := insertSourceMigrationFixture(t, ctx, pool, owner, uuid.New(), "unified_app", "export const fusedConfig = {\"name\":\"stale\"};\nexport default buildUnifiedApp({});", true, unifiedSourceMigrated)
	migrator := NewPostgresStore(pool).(UnifiedAppSourceMigrationStore)
	first, err := migrator.MigrateUnifiedAppSources(ctx)
	// The original migration timestamp must not make this export invisible to format-v2 conversion.
	if err != nil || first.MigratedRows != 1 {
		t.Fatalf("first startup=%+v %v", first, err)
	}
	assertSourceMigrationFixture(t, ctx, pool, fixture)
	state, err := NewPostgresConfigRepository(pool).GetConfigState(ctx, "unified_app:"+fixture.id.String()+":1.0.0")
	// The authenticated source repository must expose current config without redeploying or replacing the immutable receipt.
	if err != nil || state == nil || strings.Contains(string(state.DesiredState), "fusedConfig") || state.SourceHash != "sha256:unchanged" {
		t.Fatalf("editor source read failed: %v", err)
	}
	again, err := migrator.MigrateUnifiedAppSources(ctx)
	// A completed versioned migration must not revisit or rewrite this source at the next startup.
	if err != nil || again.MigratedRows != 0 || again.AlreadyOwned != 0 {
		t.Fatalf("repeat startup=%+v %v", again, err)
	}
}
