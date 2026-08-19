package unified

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestDefinitionCodecRoundTripsDependenciesAndRollback proves schema v2 keeps
// exact DAG edges, compensation identity, and private mappings executable.
func TestDefinitionCodecRoundTripsDependenciesAndRollback(t *testing.T) {
	var rootInput *Program
	dependentInput, err := CompileWithTargets(map[string]any{"id": "${response.A.id}"}, DefaultLimits(), []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	rollbackInput, err := CompileWithTargets(map[string]any{"id": "${response.A.id}"}, DefaultLimits(), []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	a := bindingFixture("A", rootInput)
	a.Rollback = &RollbackDefinition{
		OperationID: "deleteIssue", ServiceID: a.ServiceID, ServiceVersionID: a.ServiceVersionID,
		EndpointID: uuid.New(), Input: rollbackInput,
	}
	b := bindingFixture("B", dependentInput)
	b.DependsOn = []string{"A"}
	restored := mustDecodeDefinitions(t, mustEncodeDefinitions(t, []OperationDefinition{{
		Name: "issues.create", InputSchema: []byte(`{"type":"object"}`), Bindings: []BindingDefinition{b, a},
	}}))[0]
	if !reflect.DeepEqual(restored.Bindings[1].DependsOn, []string{"A"}) || restored.Bindings[0].Rollback == nil {
		t.Fatalf("restored definitions = %#v", restored.Bindings)
	}
	if restored.Bindings[0].Rollback.OperationID != "deleteIssue" || restored.Bindings[0].Rollback.EndpointID != a.Rollback.EndpointID {
		t.Fatalf("restored rollback = %#v", restored.Bindings[0].Rollback)
	}
}

// TestDefinitionCodecDefaultsOptionalGraphFields proves a minimal v2 binding
// decodes with no dependencies or rollback when those fields are absent.
func TestDefinitionCodecDefaultsOptionalGraphFields(t *testing.T) {
	program := `{"schema_version":1,"root":{"kind":"literal","literal":true}}`
	raw := `[` + validWireOperation(validWireBinding(program)) + `]`
	restored, err := DecodeDefinitions([]byte(raw), DefaultLimits())
	if err != nil {
		t.Fatalf("DecodeDefinitions() error = %v", err)
	}
	if len(restored) != 1 || len(restored[0].Bindings) != 1 || len(restored[0].Bindings[0].DependsOn) != 0 || restored[0].Bindings[0].Rollback != nil {
		t.Fatalf("decoded minimal definition = %#v", restored)
	}
}

// TestDefinitionCodecRejectsRollbackOutsideBindingScope prevents compensation
// from reusing a binding selector to cross a service/version boundary.
func TestDefinitionCodecRejectsRollbackOutsideBindingScope(t *testing.T) {
	binding := bindingFixture("A", nil)
	binding.Rollback = &RollbackDefinition{
		OperationID: "deleteIssue", ServiceID: uuid.New(),
		ServiceVersionID: binding.ServiceVersionID, EndpointID: uuid.New(),
	}
	definition := OperationDefinition{Name: "issues.create", InputSchema: []byte(`{"type":"object"}`), Bindings: []BindingDefinition{binding}}
	if _, err := EncodeDefinitions([]OperationDefinition{definition}, DefaultLimits()); !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("EncodeDefinitions() error = %v, want ErrInvalidDefinitions", err)
	}
}

// TestDefinitionCodecRejectsRollbackReferenceToAnotherTarget proves a rollback
// can consume only the compensated binding's own successful raw response.
func TestDefinitionCodecRejectsRollbackReferenceToAnotherTarget(t *testing.T) {
	foreignInput, err := CompileWithTargets("${response.B.id}", DefaultLimits(), []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	a := bindingFixture("A", nil)
	a.Rollback = &RollbackDefinition{
		OperationID: "deleteIssue", ServiceID: a.ServiceID,
		ServiceVersionID: a.ServiceVersionID, EndpointID: uuid.New(), Input: foreignInput,
	}
	b := bindingFixture("B", nil)
	definition := OperationDefinition{Name: "issues.create", InputSchema: []byte(`{"type":"object"}`), Bindings: []BindingDefinition{a, b}}
	if _, err := EncodeDefinitions([]OperationDefinition{definition}, DefaultLimits()); !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("EncodeDefinitions() error = %v, want ErrInvalidDefinitions", err)
	}
}
