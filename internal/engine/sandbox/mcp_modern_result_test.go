package sandbox

import (
	"reflect"
	"testing"
)

// TestMCPModernStructuredResultPreservesContent covers hosts that consume only the advertised structured result.
func TestMCPModernStructuredResultPreservesContent(t *testing.T) {
	// Success, tool errors, and empty results must all survive the structured-only client path.
	for _, content := range [][]any{
		{map[string]any{"type": "text", "text": `{"result":{"echo":"ok"}}`}},
		{map[string]any{"type": "text", "text": `{"error":"permission_denied"}`}},
		{},
	} {
		result := map[string]any{"content": content}
		attachMCPModernStateHandle(result, "fused-state:test")
		structured, _ := result["structuredContent"].(map[string]any)
		// Continuation metadata must not replace the actual success, error, or empty result.
		if structured["stateHandle"] != "fused-state:test" || !reflect.DeepEqual(structured["content"], content) {
			t.Fatalf("structured result lost tool content: %#v", structured)
		}
	}
}

// TestMCPModernStructuredResultSchema includes the content clients need to interpret execution results.
func TestMCPModernStructuredResultSchema(t *testing.T) {
	tool := map[string]any{"name": "execute", "inputSchema": map[string]any{"properties": map[string]any{}}}
	// A valid execute descriptor must remain adaptable without a child process.
	if err := adaptMCPModernToolList(map[string]any{"tools": []any{tool}}); err != nil {
		t.Fatal(err)
	}
	schema := tool["outputSchema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	// Closed schemas must explicitly admit both result content and the continuation handle.
	if properties["content"] == nil || !reflect.DeepEqual(schema["required"], []string{"stateHandle", "content"}) {
		t.Fatalf("output schema hides execution content: %#v", schema)
	}
}
