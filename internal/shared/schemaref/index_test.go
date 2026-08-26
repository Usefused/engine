package schemaref

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestIndexKeepsRecursiveDefinitionsShared proves references remain finite lookups instead of expanded copies.
func TestIndexKeepsRecursiveDefinitionsShared(t *testing.T) {
	definitions := map[string]json.RawMessage{
		"A": json.RawMessage(`{"type":"object","properties":{"next":{"$ref":"#/$defs/B"}}}`),
		"B": json.RawMessage(`{"allOf":[{"$ref":"#/$defs/A"},{"type":"object","properties":{"label":{"type":"string"}}}]}`),
	}
	index, err := New(definitions)
	// Forward edges and reference cycles must be admitted without a recursive expansion pass.
	if err != nil {
		t.Fatal(err)
	}
	root := json.RawMessage(`{"$ref":"#/$defs/A"}`)
	// Repeated operation lookup must not alter or embed the dictionary in the compact root.
	for iteration := 0; iteration < 100; iteration++ {
		if err := index.Validate(root, true); err != nil {
			t.Fatal(err)
		}
	}
	// Root bytes remain independent from the size or reachability of the shared graph.
	if string(root) != `{"$ref":"#/$defs/A"}` {
		t.Fatal("root expanded")
	}
}

// TestIndexReferenceScopes covers local precedence, escaped names, composition indexes, and opaque examples.
func TestIndexReferenceScopes(t *testing.T) {
	index, err := New(map[string]json.RawMessage{
		"a/b~c":   json.RawMessage(`{"allOf":[{"type":"string"}],"examples":[{"$ref":"https://not-a-schema.test"}]}`),
		"Payload": json.RawMessage(`{"type":"integer"}`),
	})
	// Reference-looking application samples must never become executable dependencies.
	if err != nil {
		t.Fatal(err)
	}
	root := map[string]any{"$defs": map[string]any{"Payload": map[string]any{"type": "boolean"}}}
	node, _, scope, ok := index.Resolve(root, "#/$defs/Payload")
	// A local document definition cannot be silently shadowed by a same-named service definition.
	if !ok || scope != "" || node.(map[string]any)["type"] != "boolean" {
		t.Fatalf("local lookup = %v %q %v", node, scope, ok)
	}
	node, _, scope, ok = index.Resolve(root, "#/$defs/a~1b~0c/allOf/0")
	// Exact pointer decoding must preserve provider names and legitimate subschema paths.
	if !ok || scope != "a/b~c" || node.(map[string]any)["type"] != "string" {
		t.Fatalf("shared lookup = %v %q %v", node, scope, ok)
	}
}

// TestIndexRejectsInvalidEdges prevents unavailable or malformed refs from weakening executable schemas.
func TestIndexRejectsInvalidEdges(t *testing.T) {
	for _, ref := range []string{"#/$defs/missing", "https://example.test/schema", "#/$defs/A~2B", "#/$defs/A/allOf/00"} {
		_, err := New(map[string]json.RawMessage{"A": json.RawMessage(`{"allOf":[true],"properties":{"value":{"$ref":` + quoted(ref) + `}}}`)})
		// Every invalid dependency must fail before a dictionary reaches a runtime cache.
		if err == nil {
			t.Fatalf("accepted %s", ref)
		}
	}
	var missing *Index
	// The marker cannot opt into shared resolution without an exact-version dictionary.
	if err := missing.Validate(json.RawMessage(`{"$ref":"#/$defs/A"}`), true); err == nil {
		t.Fatal("accepted missing dictionary")
	}
}

// TestIndexBoundsStructuralDepth protects admission even when a schema has no references.
func TestIndexBoundsStructuralDepth(t *testing.T) {
	raw := strings.Repeat(`{"items":`, MaxDepth+1) + `true` + strings.Repeat(`}`, MaxDepth+1)
	_, err := New(map[string]json.RawMessage{"deep": json.RawMessage(raw)})
	// Reference sharing cannot become a bypass around structural recursion limits.
	if err == nil {
		t.Fatal("accepted excessive depth")
	}
	_, err = New(map[string]json.RawMessage{"extra": json.RawMessage(`{} {}`)})
	// Strict one-value decoding prevents valid-prefix smuggling.
	if err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

// quoted builds JSON strings for adversarial reference fixtures without shell or hand-escaping differences.
func quoted(value string) string { raw, _ := json.Marshal(value); return string(raw) }
