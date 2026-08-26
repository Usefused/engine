package schemacontract

import (
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// sharedTestSchema produces the same independently hashed envelope as Registry publication.
func sharedTestSchema(t *testing.T, raw string, shared bool) fusedobject.SchemaContract {
	t.Helper()
	hash, err := canonicaljson.HexSchemaSHA256([]byte(raw))
	// Test fixtures must not hide invalid canonical source behind a fabricated hash.
	if err != nil {
		t.Fatal(err)
	}
	return fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: json.RawMessage(raw), ContentHash: hash, SharedDefinitions: shared}
}

// TestPrepareSnapshotBindsDefinitionsOnce verifies all roots share one version-specific admitted dictionary.
func TestPrepareSnapshotBindsDefinitionsOnce(t *testing.T) {
	metadata := fusedobject.ServiceMetadata{SchemaDefinitions: map[string]fusedobject.SchemaContract{"Payload": sharedTestSchema(t, `{"type":"object","properties":{"label":{"type":"string"}}}`, false)}}
	metadata.RequiredCapabilities = []string{fusedobject.ExecutionCapabilityJSONSchemaSharedDefinitionsV1}
	root := sharedTestSchema(t, `{"$ref":"#/$defs/Payload"}`, true)
	endpoints := []fusedobject.Endpoint{{Parameters: fusedobject.Parameters{{Schema: &root}}}}
	// Hashes and dependencies are admitted before any consumer receives runtime-only pointers.
	if err := PrepareSnapshot(&metadata, endpoints, nil); err != nil {
		t.Fatal(err)
	}
	// An attachment is a shared pointer, not a second serialized definition payload.
	if root.DefinitionIndex != metadata.DefinitionIndex {
		t.Fatal("dictionary not shared")
	}
	raw, err := json.Marshal(root)
	// Runtime attachments must never leak into operation JSON persistence or transport.
	if err != nil || string(raw) == "" {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if _, exists := decoded["DefinitionIndex"]; exists {
		t.Fatal("index serialized")
	}
}

// TestPrepareSnapshotRejectsCorruptionAndMissingCapability prevents metadata from bypassing negotiation or integrity checks.
func TestPrepareSnapshotRejectsCorruptionAndMissingCapability(t *testing.T) {
	root := sharedTestSchema(t, `{"$ref":"#/$defs/Payload"}`, true)
	definition := sharedTestSchema(t, `{"type":"string"}`, false)
	metadata := fusedobject.ServiceMetadata{SchemaDefinitions: map[string]fusedobject.SchemaContract{"Payload": definition}}
	endpoints := []fusedobject.Endpoint{{Parameters: fusedobject.Parameters{{Schema: &root}}}}
	// Unused declarations may exist, but a referencing operation must explicitly negotiate shared lookup.
	if err := PrepareSnapshot(&metadata, endpoints, nil); err == nil {
		t.Fatal("accepted missing capability")
	}
	metadata.RequiredCapabilities = []string{fusedobject.ExecutionCapabilityJSONSchemaSharedDefinitionsV1}
	definition.Raw = json.RawMessage(`{"type":"integer"}`)
	metadata.SchemaDefinitions["Payload"] = definition
	// The complete snapshot fails even when only a shared definition's raw bytes were tampered with.
	if err := PrepareSnapshot(&metadata, endpoints, nil); err == nil {
		t.Fatal("accepted corrupt definition")
	}
}
