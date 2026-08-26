package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// sharedMCPSchema hashes bounded source truth independently from the compact referencing operation.
func sharedMCPSchema(t *testing.T, raw string) fusedobject.SchemaContract {
	t.Helper()
	hash, err := canonicaljson.HexSchemaSHA256([]byte(raw))
	// A fixture's hash must exercise the actual Engine validation boundary.
	if err != nil {
		t.Fatal(err)
	}
	return fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: json.RawMessage(raw), ContentHash: hash}
}

// TestMCPSharedSchemasAreStoredOnceAndVersionScoped proves compact roots cannot duplicate or cross-bind definitions.
func TestMCPSharedSchemasAreStoredOnceAndVersionScoped(t *testing.T) {
	fixture := &Fixture{SchemaDefinitions: map[string]map[string]fusedobject.SchemaContract{"version-a": {"Payload": sharedMCPSchema(t, `{"type":"object","properties":{"distinct_field":{"type":"string"}}}`)}}}
	for position := 0; position < 100; position++ {
		fixture.Operations = append(fixture.Operations, FixtureOperation{ServiceVersionID: "version-a", Parameters: []models.Parameter{{Schema: &models.SchemaContract{Raw: json.RawMessage(`{"$ref":"#/$defs/Payload"}`), SharedDefinitions: true}}}})
	}
	// The same definition is charged once rather than copied into every operation's schema limit budget.
	if err := validateMCPFixtureSchemas(fixture); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(fixture)
	// Runtime-only indexes must not introduce extra dictionary copies into serialized fixture data.
	if err != nil || strings.Count(string(payload), "distinct_field") != 1 {
		t.Fatalf("dictionary duplicated: %v", err)
	}
	fixture.Operations[0].ServiceVersionID = "version-b"
	// A same-named schema from another version cannot satisfy this operation's reference.
	if err := validateMCPFixtureSchemas(fixture); err != ErrMCPSchemaInvalid {
		t.Fatalf("cross-version result = %v", err)
	}
}

// TestMCPSharedDefinitionLimitsCannotBeBypassed ensures compact roots still pay for their saved schema complexity.
func TestMCPSharedDefinitionLimitsCannotBeBypassed(t *testing.T) {
	definition := sharedMCPSchema(t, `{"type":"string","description":"`+strings.Repeat("x", maxMCPSchemaEncodedBytes)+`"}`)
	fixture := &Fixture{SchemaDefinitions: map[string]map[string]fusedobject.SchemaContract{"v1": {"Payload": definition}}}
	// A small reference cannot hide an oversized definition from the existing per-schema ceiling.
	if err := validateMCPFixtureSchemas(fixture); err != ErrMCPSchemaEncodedBytesLimit {
		t.Fatalf("oversized result = %v", err)
	}
}
