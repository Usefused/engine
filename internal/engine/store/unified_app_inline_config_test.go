package store

import (
	"encoding/json"
	"testing"
)

// TestInlineConfigRemainsSourceOwned protects new app definitions from a competing export during restart migrations.
func TestInlineConfigRemainsSourceOwned(t *testing.T) {
	for _, source := range []string{
		`export default buildUnifiedApp({config:{"name":"customer"},input:z.object({}),execute(){return {};}});`,
		`export default buildUnifiedApp({input:z.object({config:z.string()}),config:{"name":"customer"},execute(){return {};}});`,
		"/* export const fusedConfig = {}; */\nexport default buildUnifiedApp({config:{\"name\":\"customer\"}});",
	} {
		desired, err := json.Marshal(map[string]any{"kind": "unified_app", "name": "customer", "source": source})
		// Only encoding errors are possible here; metadata detection must remain entirely static.
		if err != nil {
			t.Fatal(err)
		}
		converted, status := migratedUnifiedAppSource(desired)
		// Source-owned input is already portable and must not be rewritten during startup.
		if status != unifiedSourceAlreadyOwned || converted != "" {
			t.Fatalf("status=%v converted=%q", status, converted)
		}
	}
}
