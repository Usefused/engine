package sandbox

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/schemacontract"
)

// capabilitySharedSchemaOperation models an admitted version whose request references its shared dictionary.
func capabilitySharedSchemaOperation(t *testing.T) ResolvedPhysicalOperation {
	t.Helper()
	_, operation := physicalExecutionTestOperation("https://provider.invalid")
	root := sharedMCPSchema(t, `{"$ref":"#/$defs/CustomerInput"}`)
	root.SharedDefinitions = true
	operation.match.endpoint.RequestContent = &fusedobject.RequestContent{
		Representations: []fusedobject.RequestRepresentation{{
			MediaType: "application/x-www-form-urlencoded", Serialization: fusedobject.RequestSerializationForm, Schema: &root,
		}},
	}
	operation.match.service.SchemaDefinitions = map[string]fusedobject.SchemaContract{
		"CustomerInput": sharedMCPSchema(t, `{"type":"object","properties":{"email":{"type":"string"}},"additionalProperties":false}`),
	}
	// The cache owns one validated index per exact service version before calls are admitted.
	if err := schemacontract.PrepareSnapshot(operation.match.service, []fusedobject.Endpoint{operation.match.endpoint}, nil); err != nil {
		t.Fatal(err)
	}
	return operation
}

// TestCapabilitySharedSchemasSurviveFixtureConversion reproduces the lost index on a Stripe-style form body.
func TestCapabilitySharedSchemasSurviveFixtureConversion(t *testing.T) {
	operation := capabilitySharedSchemaOperation(t)
	fixture, err := capabilityFixtureOperation(operation)
	// An admitted compact request must remain executable after the fixture's JSON conversion.
	if err != nil {
		t.Fatal(err)
	}
	// Referenced body fields must be accepted using canonical truth, not a permissive projection fallback.
	if err := validateCallParams(fixture, map[string]any{"email": "customer@example.com"}); err != nil {
		t.Fatalf("valid referenced form input rejected: %v", err)
	}
	// Reusing the immutable version index avoids compiling or copying its dictionary for every call.
	if fixture.RequestContent.Representations[0].Schema.DefinitionIndex != operation.match.service.DefinitionIndex {
		t.Fatal("capability fixture did not reuse its exact service version's index")
	}
	// Rebinding must not broaden the provider contract to accept undeclared body fields.
	if err := validateCallParams(fixture, map[string]any{"unexpected": "value"}); err == nil {
		t.Fatal("undeclared form field was accepted")
	}
}

// TestCapabilitySharedSchemasRejectMissingIndex keeps incomplete snapshots from reaching provider dispatch.
func TestCapabilitySharedSchemasRejectMissingIndex(t *testing.T) {
	operation := capabilitySharedSchemaOperation(t)
	operation.match.service.DefinitionIndex = nil
	_, err := capabilityFixtureOperation(operation)
	// An endpoint's stale pointer cannot substitute for its resolved version's missing dictionary.
	if !errors.Is(err, ErrMCPSchemaInvalid) {
		t.Fatalf("missing dictionary error = %v, want schema rejection", err)
	}
}

// TestCapabilitySharedSchemasRejectDanglingReference prevents unrelated version definitions from satisfying a root.
func TestCapabilitySharedSchemasRejectDanglingReference(t *testing.T) {
	operation := capabilitySharedSchemaOperation(t)
	other := &fusedobject.ServiceMetadata{SchemaDefinitions: map[string]fusedobject.SchemaContract{
		"OtherInput": sharedMCPSchema(t, `{"type":"object"}`),
	}}
	// The alternate dictionary is valid itself but cannot resolve this operation's exact reference.
	if err := schemacontract.PrepareDefinitions(other); err != nil {
		t.Fatal(err)
	}
	operation.match.service.DefinitionIndex = other.DefinitionIndex
	_, err := capabilityFixtureOperation(operation)
	// Dictionary admission alone must never imply that every operation root resolves against it.
	if !errors.Is(err, ErrMCPSchemaInvalid) {
		t.Fatalf("dangling reference error = %v, want schema rejection", err)
	}
}
