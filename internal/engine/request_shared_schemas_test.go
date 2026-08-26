package engine

import (
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemaref"
)

// TestRequestBodyDeclarationUsesSharedDefinitions keeps strict argument discovery working without expanded raw schemas.
func TestRequestBodyDeclarationUsesSharedDefinitions(t *testing.T) {
	index, err := schemaref.New(map[string]json.RawMessage{
		"Payload": json.RawMessage(`{"allOf":[{"$ref":"#/$defs/Base"},{"type":"object","properties":{"count":{"type":"integer"}}}]}`),
		"Base":    json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"additionalProperties":false}`),
	})
	// The service dictionary is admitted once independently of operation selection.
	if err != nil {
		t.Fatal(err)
	}
	contract := &models.SchemaContract{Raw: json.RawMessage(`{"$ref":"#/$defs/Payload"}`), SharedDefinitions: true, DefinitionIndex: index}
	contract.Projection.Properties = map[string]models.Schema{"not_declared": {Type: "string"}}
	fields, additional, err := requestBodyDeclaration(contract)
	// Root composition must permit the declared fields, not arbitrary nested or unknown values.
	if err != nil || additional || len(fields) != 2 {
		t.Fatalf("fields=%v additional=%v err=%v", fields, additional, err)
	}
	for _, name := range []string{"label", "count"} {
		if _, exists := fields[name]; !exists {
			t.Fatalf("missing %s", name)
		}
	}
	contract.DefinitionIndex = nil
	// Missing attachments must fail instead of silently treating a compact root as an empty object.
	if _, _, err := requestBodyDeclaration(contract); err == nil {
		t.Fatal("accepted missing dictionary")
	}
}

// TestRequestBodyDeclarationPreservesBooleanTruth keeps explicit unconstrained schemas distinct from omitted projections.
func TestRequestBodyDeclarationPreservesBooleanTruth(t *testing.T) {
	_, additional, err := requestBodyDeclaration(&models.SchemaContract{Raw: json.RawMessage(`true`)})
	// Literal true is authoritative permission for arbitrary body fields, even with an empty compatibility projection.
	if err != nil || !additional {
		t.Fatalf("true schema additional=%v err=%v", additional, err)
	}
}
