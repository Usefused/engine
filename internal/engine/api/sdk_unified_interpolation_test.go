package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/engine/unified"
)

// TestCompileSDKUnifiedInterpolationPersistsPrivateTemplate proves ordinary
// sdk.yaml compilation accepts mixed strings without exposing them publicly.
func TestCompileSDKUnifiedInterpolationPersistsPrivateTemplate(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	github := operation.Bindings["github"]
	github.Input = json.RawMessage(`{"title":"Issue ${input.title}"}`)
	operation.Bindings["github"] = github
	fixture.document.UnifiedOperations["issues.create"] = operation

	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatalf("compileSDKUnifiedOperations() error = %v", err)
	}
	definition := mustDecodeSingleUnifiedDefinition(t, compiled.DefinitionJSON, 2)
	var program *unified.Program
	for _, binding := range definition.Bindings {
		if binding.PublicTarget == "github" {
			program = binding.Input
		}
	}
	if program == nil {
		t.Fatal("compiled github input is missing")
	}
	got, err := program.Evaluate(unified.EvaluationContext{Input: map[string]any{"title": "Connection failed"}})
	want := map[string]any{"title": "Issue Connection failed"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate() = %#v, %v, want %#v", got, err, want)
	}
	assertUnifiedDescriptorOmits(t, compiled.Descriptors, "Issue ${input.title}")
}

// TestUnifiedSchedulerRejectsCompositeInterpolationBeforeDispatch proves a
// runtime array cannot be stringified implicitly or reach a provider call.
func TestUnifiedSchedulerRejectsCompositeInterpolationBeforeDispatch(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	target := preparedUnifiedTargetFixture(t, "A", nil, false)
	target.input = mustCompileUnifiedProgram(t, map[string]any{"value": "items=${input.items}"}, nil)
	call := preparedUnifiedCallFixture(t, target)
	call.input = map[string]any{"items": []any{"one", "two"}}

	response := executePreparedUnifiedCallWithCall(runtime, call)
	if len(response.Results) != 1 || response.Results[0].GetErrorCode() != "input_mapping_failed" {
		t.Fatalf("response = %#v", response)
	}
	assertScriptedCalls(t, runtime, nil, nil)
}
