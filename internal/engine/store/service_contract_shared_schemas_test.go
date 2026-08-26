package store

import (
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// storeSharedSchema builds independently verifiable envelopes for persistence hash tests.
func storeSharedSchema(t *testing.T, raw string, shared bool) fusedobject.SchemaContract {
	t.Helper()
	hash, err := canonicaljson.HexSchemaSHA256([]byte(raw))
	// Tests must not conceal canonical-number changes behind a hard-coded digest.
	if err != nil {
		t.Fatal(err)
	}
	return fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: json.RawMessage(raw), ContentHash: hash, SharedDefinitions: shared}
}

// TestServiceContractHashIncludesSharedDefinitions preserves immutable version identity when only a definition changes.
func TestServiceContractHashIncludesSharedDefinitions(t *testing.T) {
	root := storeSharedSchema(t, `{"$ref":"#/$defs/Limit"}`, true)
	snapshot := ServiceContractSnapshot{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(), ServiceMetadata: fusedobject.ServiceMetadata{SchemaDefinitions: map[string]fusedobject.SchemaContract{"Limit": storeSharedSchema(t, `{"type":"number","maximum":1.0}`, false)}}, Endpoints: []fusedobject.Endpoint{{Parameters: fusedobject.Parameters{{Schema: &root}}}}}
	first, err := serviceContractHash(snapshot)
	// Shared roots must be hashable without copying their full closure into the operation.
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ServiceMetadata.SchemaDefinitions["Limit"] = storeSharedSchema(t, `{"maximum":1e0,"type":"number"}`, false)
	equivalent, err := serviceContractHash(snapshot)
	// JSONB spelling/order changes must not manufacture a new immutable contract identity.
	if err != nil || equivalent != first {
		t.Fatalf("equivalent hash mismatch: %v", err)
	}
	snapshot.ServiceMetadata.SchemaDefinitions["Limit"] = storeSharedSchema(t, `{"maximum":2,"type":"number"}`, false)
	changed, err := serviceContractHash(snapshot)
	// A changed referenced constraint must affect the complete snapshot even when operation bytes are unchanged.
	if err != nil || changed == first {
		t.Fatalf("definition change ignored: %v", err)
	}
}
