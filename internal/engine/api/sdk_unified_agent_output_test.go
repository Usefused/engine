package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/engine/unified"
)

// TestUnifiedAgentOutputUsesSharedProjection verifies useful final objects without provider-specific MCP mapping code.
func TestUnifiedAgentOutputUsesSharedProjection(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object","required":["email","file"],"properties":{
			"email":{"type":"object","required":["id"],"properties":{
				"id":"${response.gmail_detail.id}",
				"snippet":"${response.gmail_detail.snippet?}",
				"headers":{"type":"array","value":"${response.gmail_detail.payload.headers?}","items":{
					"type":"object","properties":{"name":{"type":"string"},"value":{"type":"string"}}
				}}
			}},
			"file":{"type":"object","required":["id"],"properties":{
				"id":"${response.drive_detail.id}","name":"${response.drive_detail.name?}"
			}}
		}
	}`)
	schemaJSON, source, err := compileSDKUnifiedOutputDocument(raw, []string{"gmail_list", "gmail_detail", "drive_list", "drive_detail"})
	// Both adapters rely on this compiler rather than a separate MCP response normalizer.
	if err != nil {
		t.Fatal(err)
	}
	program, err := compileSDKUnifiedDynamicValue(source.raw, source.allowedTargets)
	// Mapping compilation must succeed independently of any provider or connected account.
	if err != nil {
		t.Fatal(err)
	}
	headers := []any{map[string]any{"name": "Date", "value": "synthetic date"}, map[string]any{"name": "Subject", "value": "synthetic subject"}}
	mapped, err := program.Evaluate(unified.EvaluationContext{Responses: map[string]any{
		"gmail_list":   map[string]any{"messages": []any{map[string]any{"id": "fixture-email"}}},
		"drive_list":   map[string]any{"files": []any{map[string]any{"id": "fixture-file"}}},
		"gmail_detail": map[string]any{"id": "fixture-email", "payload": map[string]any{"headers": headers}, "raw": "omitted-fixture"},
		"drive_detail": map[string]any{"id": "fixture-file", "name": "fixture.txt", "unused": "omitted-fixture"},
	}})
	// Optional missing snippet must be omitted while named final objects remain valid.
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"email": map[string]any{"id": "fixture-email", "headers": headers},
		"file":  map[string]any{"id": "fixture-file", "name": "fixture.txt"},
	}
	// Exact projection excludes raw payloads and list dependencies, without relying on header order.
	if !reflect.DeepEqual(mapped, want) {
		t.Fatal("shared output projection differs from the authored final objects")
	}
	schema, err := compileUnifiedSchema(schemaJSON)
	// Discovered output schemas must agree with the values consumed by both SDK and MCP clients.
	if err != nil {
		t.Fatal(err)
	}
	// This also verifies that selected parsed header arrays need no new mapping grammar.
	if err := schema.VisitJSON(mapped); err != nil {
		t.Fatal(err)
	}
}
