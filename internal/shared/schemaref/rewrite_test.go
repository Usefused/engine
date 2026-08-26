package schemaref

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRewriteReferencesPreservesOpaqueData distinguishes executable schema
// edges from reference-like provider examples and extension payloads.
func TestRewriteReferencesPreservesOpaqueData(t *testing.T) {
	raw := `{"$id":"https://old-schema.example/root","$ref":"#/$defs/A","$defs":{"A":{"type":"object","properties":{"examples":{"$ref":"#/$defs/A"}}}},"default":{"$ref":"opaque","$id":"opaque-id"},"examples":[{"$ref":"opaque"}],"x-provider":{"$ref":"opaque"}}`
	root, err := decode(json.RawMessage(raw))
	// Fixtures must be valid JSON before reference relocation is exercised.
	if err != nil {
		t.Fatal(err)
	}
	visited := 0
	result, err := RewriteReferences(root, func(ref string) (string, error) {
		visited++
		return "#/components/schemas/A", nil
	})
	// Only the schema-root and actual property-schema edge should be rewritten.
	if err != nil || visited != 2 {
		t.Fatalf("rewrite error=%v, references=%d", err, visited)
	}
	encoded, err := json.Marshal(result)
	// Opaque data must remain byte-equivalent in meaning and retain its literal ref.
	if err != nil || strings.Count(string(encoded), `"$ref":"opaque"`) != 3 {
		t.Fatalf("opaque data changed: %s, error=%v", encoded, err)
	}
	// Schema scope moves with relocated references, but application identifiers do not.
	if strings.Contains(string(encoded), "old-schema.example") || !strings.Contains(string(encoded), "opaque-id") {
		t.Fatal("schema identity rebasing changed opaque application data")
	}
	original, _ := json.Marshal(root)
	// Relocation must not mutate an immutable dictionary shared by other consumers.
	if strings.Contains(string(original), "#/components/schemas/") {
		t.Fatal("reference relocation mutated the source dictionary")
	}
}

// TestRewriteReferencesRejectsUnboundedStructures proves relocation does not
// turn cyclic in-memory schema containers into unbounded recursive work.
func TestRewriteReferencesRejectsUnboundedStructures(t *testing.T) {
	root := make(map[string]any)
	root["properties"] = map[string]any{"again": root}
	_, err := RewriteReferences(root, func(ref string) (string, error) { return ref, nil })
	// Real schema-reference cycles are strings; structural Go pointer cycles are invalid.
	if err == nil {
		t.Fatal("structurally cyclic schema accepted")
	}
}
