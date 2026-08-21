package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/engine/unified"
)

func TestCompileSDKUnifiedOutputDocumentBuildsRecursiveProjection(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"required":["name","customer","files"],
		"properties":{
			"name":"${response.drive.name}",
			"active":{"type":"boolean","value":"${response.drive.active}"},
			"customer":{
				"type":"object",
				"value":"${response.drive.customer}",
				"properties":{"id":{"type":"string"},"label":{"type":"string"}},
				"required":["id"]
			},
			"files":{
				"type":"array",
				"value":"${response.drive.files}",
				"items":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}
			}
		}
	}`)

	schemaJSON, source, err := compileSDKUnifiedOutputDocument(raw, []string{"drive"})
	if err != nil {
		t.Fatalf("compileSDKUnifiedOutputDocument() error = %v", err)
	}
	program, err := compileSDKUnifiedDynamicValue(source.raw, source.allowedTargets)
	if err != nil {
		t.Fatalf("compile output mapping: %v", err)
	}
	mapped, err := program.Evaluate(unified.EvaluationContext{Responses: map[string]any{
		"drive": map[string]any{
			"name": "report.pdf", "active": true,
			"customer": map[string]any{"id": "customer-1", "label": "Creative Joe"},
			"files":    []any{map[string]any{"id": "file-1"}},
		},
	}})
	if err != nil {
		t.Fatalf("evaluate output mapping: %v", err)
	}
	want := map[string]any{
		"name": "report.pdf", "active": true,
		"customer": map[string]any{"id": "customer-1", "label": "Creative Joe"},
		"files":    []any{map[string]any{"id": "file-1"}},
	}
	if !reflect.DeepEqual(mapped, want) {
		t.Fatalf("mapped = %#v, want %#v", mapped, want)
	}
	schema, err := compileUnifiedSchema(schemaJSON)
	if err != nil {
		t.Fatalf("compile projected schema: %v", err)
	}
	if err := schema.VisitJSON(mapped); err != nil {
		t.Fatalf("projected schema rejected mapped output: %v", err)
	}
	invalidValues := []map[string]any{
		{
			"name": "report.pdf", "active": true,
			"customer": map[string]any{"label": "missing id"},
			"files":    []any{map[string]any{"id": "file-1"}},
		},
		{
			"name": "report.pdf", "active": true,
			"customer": map[string]any{"id": "customer-1"},
			"files":    []any{map[string]any{"name": "missing id"}},
		},
	}
	for index, invalid := range invalidValues {
		if err := schema.VisitJSON(invalid); err == nil {
			t.Fatalf("schema accepted invalid pass-through value %d: %#v", index, invalid)
		}
	}
}

func TestCompileSDKUnifiedOutputDocumentRequiredRemainsSchemaValidation(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{"name":"${response.drive.name?}"},
		"required":["name"]
	}`)
	schemaJSON, source, err := compileSDKUnifiedOutputDocument(raw, []string{"drive"})
	if err != nil {
		t.Fatal(err)
	}
	program, err := compileSDKUnifiedDynamicValue(source.raw, source.allowedTargets)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := program.Evaluate(unified.EvaluationContext{Responses: map[string]any{"drive": map[string]any{}}})
	if err != nil {
		t.Fatalf("optional property evaluation failed before schema validation: %v", err)
	}
	schema, err := compileUnifiedSchema(schemaJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.VisitJSON(mapped); err == nil {
		t.Fatal("required output property was accepted after optional mapping omitted it")
	}
}

func TestCompileSDKUnifiedOutputDocumentRejectsRemovedAndInvalidRootForms(t *testing.T) {
	tests := []json.RawMessage{
		json.RawMessage(`{"schema":{"type":"object"},"mapping":{"name":"${response.drive.name}"}}`),
		json.RawMessage(`{"type":"string","value":"${response.drive.name}"}`),
		json.RawMessage(`{"type":"object","value":"${response.drive}"}`),
		json.RawMessage(`{"type":"object","properties":{},"value":"${response.drive}"}`),
	}
	for index, raw := range tests {
		if _, _, err := compileSDKUnifiedOutputDocument(raw, []string{"drive"}); err == nil {
			t.Fatalf("case %d accepted removed or invalid root form: %s", index, raw)
		}
	}
	if _, err := compileSDKUnifiedOutput(tests[0], []string{"drive"}); err == nil {
		t.Fatal("public compiler accepted removed schema/mapping authoring")
	} else {
		assertWorkspaceErrorCode(t, err, "output_definition_invalid")
	}
}

func TestCompileSDKUnifiedOutputDocumentRejectsMalformedRecursiveNodes(t *testing.T) {
	tests := []json.RawMessage{
		json.RawMessage(`{"type":"object","properties":{"id":"${response.drive.id}"},"required":null}`),
		json.RawMessage(`{"type":"object","properties":{"item":{"type":"object","value":"${response.drive}","properties":{"id":{"type":"identifier"}}}}}`),
		json.RawMessage(`{"type":"object","properties":{"item":{"type":"object","value":"${response.drive}","properties":{"id":{"type":"string","format":"uuid"}}}}}`),
		json.RawMessage(`{"type":"object","properties":{"item":{"type":"object","value":"${response.drive}","properties":{"id":{"type":"string","properties":{}}}}}}`),
		json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","value":"${response.drive.items}","required":["id"]}}}`),
		json.RawMessage(`{"type":"object","properties":{"item":{"type":"object","value":"${response.drive}","items":{"type":"string"}}}}`),
		json.RawMessage(`{"type":"object","properties":{"item":{"type":"object","value":"${response.drive}","additionalProperties":"yes"}}}`),
		json.RawMessage(`{"type":"object","properties":{"item":{"id":"${response.drive.id}"}}}`),
		json.RawMessage(`{"type":"object","properties":{"items":["${response.drive.id}"]}}`),
	}
	for index, raw := range tests {
		if _, _, err := compileSDKUnifiedOutputDocument(raw, []string{"drive"}); err == nil {
			t.Fatalf("case %d accepted malformed recursive output: %s", index, raw)
		}
	}
}

func TestCompileSDKUnifiedOperationsAllowsBindingAndRootProjection(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	crm := operation.Bindings["@acme/custom-crm"]
	crm.Output = json.RawMessage(`{
		"type":"object",
		"properties":{"ticketId":"${response.@acme/custom-crm.iid}"},
		"required":["ticketId"]
	}`)
	operation.Bindings["@acme/custom-crm"] = crm
	operation.Output = json.RawMessage(`{
		"type":"object",
		"properties":{"id":"${response.github.id ?? response.@acme/custom-crm.ticketId}"},
		"required":["id"]
	}`)
	fixture.document.UnifiedOperations["issues.create"] = operation

	compiled, err := compileSDKUnifiedOperations(
		t.Context(), fixture.store, fixture.document, fixture.selections, fixture.services,
	)
	if err != nil {
		t.Fatalf("compileSDKUnifiedOperations() error = %v", err)
	}
	definition := mustDecodeSingleUnifiedDefinition(t, compiled.DefinitionJSON, 2)
	if definition.Output == nil || definition.Bindings[0].Output == nil {
		t.Fatalf("compiled outputs = root:%#v binding:%#v", definition.Output, definition.Bindings[0].Output)
	}
	descriptor := compiled.Descriptors.Operations[0]
	if len(descriptor.OutputSchema) == 0 || len(descriptor.Targets[0].OutputSchema) == 0 {
		t.Fatalf("descriptor outputs = root:%s binding:%s", descriptor.OutputSchema, descriptor.Targets[0].OutputSchema)
	}
}
