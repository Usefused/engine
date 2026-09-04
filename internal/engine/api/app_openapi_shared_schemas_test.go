package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemaref"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

type appOpenAPISharedStore struct {
	*appOpenAPITestStore
	metadata      map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata
	metadataCalls int
	metadataRefs  []store.ServiceContractMetadataRef
}

// ListServiceContractMetadata records exact batched identities without exposing
// a per-operation read fallback to the export implementation.
func (s *appOpenAPISharedStore) ListServiceContractMetadata(_ context.Context, refs []store.ServiceContractMetadataRef) (map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	s.metadataCalls++
	s.metadataRefs = append([]store.ServiceContractMetadataRef(nil), refs...)
	return s.metadata, nil
}

// TestAppOpenAPISharedSchemasReuseDefinitions preserves recursive definitions
// once across operations and keeps precision and opaque provider data intact.
func TestAppOpenAPISharedSchemasReuseDefinitions(t *testing.T) {
	s, owner := newSharedOpenAPIFixture(t, 3)
	original := string(owner.SchemaDefinitions["Node"].Raw)
	encoded, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "")
	// A finite reference cycle must export successfully rather than become {}.
	if err != nil {
		t.Fatal(err)
	}
	assertSharedOpenAPIValid(t, encoded)
	document := decodeSharedOpenAPI(t, encoded)
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	definitionKey := "Version_" + strings.ReplaceAll(owner.ServiceVersionID.String(), "-", "") + "_Definition_" + openAPISchemaDigest("Node")
	definition := components[definitionKey].(map[string]any)
	child := definition["properties"].(map[string]any)["child"].(map[string]any)
	// The cycle points to the one exported component, not another expanded copy.
	if child["$ref"] != "#/components/schemas/"+definitionKey || countSharedOpenAPIDefinitions(components) != 1 {
		t.Fatalf("shared recursive component duplicated or weakened: %#v", child)
	}
	// Multiple operations of one version must issue exactly one dictionary batch.
	if s.metadataCalls != 1 || len(s.metadataRefs) != 1 {
		t.Fatalf("metadata calls=%d refs=%d", s.metadataCalls, len(s.metadataRefs))
	}
	// Unselected definitions stay out of bounded exports, and exact numbers survive.
	if strings.Contains(string(encoded), "unused-provider-definition") || !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatal("definition selection or numeric fidelity changed")
	}
	// Examples are application data even when they look like schema references.
	if !strings.Contains(string(encoded), `"$ref":"opaque-example"`) || string(owner.SchemaDefinitions["Node"].Raw) != original {
		t.Fatal("opaque example or immutable source schema changed")
	}
	assertSharedOpenAPIFlatInput(t, document, "node0")
}

// assertSharedOpenAPIValid exercises ordinary OpenAPI tooling against the
// exported references rather than relying only on internal map assertions.
func assertSharedOpenAPIValid(t *testing.T, encoded []byte) {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromData(encoded)
	// The loader must resolve recursive component references without external I/O.
	if err != nil {
		t.Fatalf("load shared-schema OpenAPI: %v", err)
	}
	// The produced document must retain a valid standard OpenAPI contract.
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate shared-schema OpenAPI: %v", err)
	}
}

// assertSharedOpenAPIFlatInput verifies reference preservation does not change
// the flat input shape actually accepted by Engine execution.
func assertSharedOpenAPIFlatInput(t *testing.T, document map[string]any, operation string) {
	t.Helper()
	request := openAPITestOperationRequest(t, document, operation)
	input := request["properties"].(map[string]any)["input"].(map[string]any)
	// Root-field lookup must still expose the exact flat Engine request contract.
	if _, present := input["properties"].(map[string]any)["label"]; !present {
		t.Fatal("shared object body fields were not flattened")
	}
}

// TestAppOpenAPISharedSchemasPinVersions prevents identically named definitions
// in different selected service versions from overwriting each other.
func TestAppOpenAPISharedSchemasPinVersions(t *testing.T) {
	s, first := newSharedOpenAPIFixture(t, 1)
	selections, err := decodeAppOpenAPISelections(s.app.Selections)
	// The fixture owns one valid immutable selection before adding its sibling.
	if err != nil {
		t.Fatal(err)
	}
	secondRef := store.ServiceContractMetadataRef{ServiceID: first.ID, ServiceVersionID: uuid.New()}
	second := *first
	second.ServiceVersionID = secondRef.ServiceVersionID
	second.SchemaDefinitions = map[string]fusedobject.SchemaContract{"Node": sharedOpenAPIDefinition(t, `{"type":"object","properties":{"older_only":{"type":"boolean"}}}`)}
	s.metadata[secondRef] = &second
	endpoint := s.matches[0].Endpoint
	endpoint.ID, endpoint.Name = uuid.New(), "olderNode"
	s.matches = append(s.matches, store.ServiceContractEndpointMatch{SelectionIndex: 1, Endpoint: endpoint})
	selections = append(selections, models.SDKSelection{ServiceID: secondRef.ServiceID, ServiceVersionID: secondRef.ServiceVersionID, SchemaVersion: models.AppSelectionSchemaVersion, EndpointIDs: []uuid.UUID{endpoint.ID}, OperationNames: []string{endpoint.Name}})
	s.app.Selections, _ = json.Marshal(selections)
	encoded, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "")
	// Each exact version must retain its independently authored definition body.
	if err != nil {
		t.Fatal(err)
	}
	document := decodeSharedOpenAPI(t, encoded)
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	// Version namespaces must coexist while the store query remains set-based.
	if countSharedOpenAPIDefinitions(components) != 2 || s.metadataCalls != 1 || len(s.metadataRefs) != 2 {
		t.Fatal("version-scoped schema identity or batching was lost")
	}
	request := openAPITestOperationRequest(t, document, endpoint.Name)
	properties := request["properties"].(map[string]any)["input"].(map[string]any)["properties"].(map[string]any)
	// The older selected version must not inherit the current version's fields.
	if _, present := properties["older_only"]; !present {
		t.Fatal("older version schema was replaced")
	}
}

// TestAppOpenAPISharedSchemasFailClosed rejects missing dictionaries instead
// of returning a successful unconstrained contract after a failed lookup.
func TestAppOpenAPISharedSchemasFailClosed(t *testing.T) {
	s, _ := newSharedOpenAPIFixture(t, 1)
	s.metadata = nil
	_, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "")
	// Missing metadata is never an optional projection fallback for a compact root.
	var contractErr workspaceConfigHTTPError
	if !errors.As(err, &contractErr) || contractErr.code != "app_openapi_schema_unavailable" {
		t.Fatalf("missing shared definition dictionary error = %#v", err)
	}
}

// TestAppOpenAPISharedSchemasPreserveLocalScope keeps a local definition ahead
// of a same-named version dictionary entry and preserves its recursive pointer.
func TestAppOpenAPISharedSchemasPreserveLocalScope(t *testing.T) {
	s, _ := newSharedOpenAPIFixture(t, 1)
	contract := s.matches[0].Endpoint.RequestContent.Representations[0].Schema
	contract.Raw = json.RawMessage(`{"$ref":"#/$defs/Node","$defs":{"Node":{"type":"object","properties":{"local_label":{"type":"integer"},"child":{"$ref":"#/$defs/Node"}}}}}`)
	encoded, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "")
	// Relocation may lift a local target but must not bind it to the shared namesake.
	if err != nil {
		t.Fatal(err)
	}
	document := decodeSharedOpenAPI(t, encoded)
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	request := openAPITestOperationRequest(t, document, "node0")
	properties := request["properties"].(map[string]any)["input"].(map[string]any)["properties"].(map[string]any)
	// Only the local body graph is reachable from this otherwise compact contract.
	if _, present := properties["local_label"]; !present || countSharedOpenAPIDefinitions(components) != 0 {
		t.Fatal("local schema scope was replaced by a shared definition")
	}
	ref := properties["child"].(map[string]any)["$ref"].(string)
	resolved, found := schemaref.ResolveLocal(document, ref)
	// A recursive local property must continue to point at the complete local schema.
	if !found || resolved.(map[string]any)["properties"].(map[string]any)["local_label"] == nil {
		t.Fatal("local recursion was weakened during relocation")
	}
}

// TestAppOpenAPISharedSchemasHonorSubschemaPointers preserves suffixes through
// shared dictionary references, including array indexes in composition paths.
func TestAppOpenAPISharedSchemasHonorSubschemaPointers(t *testing.T) {
	s, owner := newSharedOpenAPIFixture(t, 1)
	owner.SchemaDefinitions["Node"] = sharedOpenAPIDefinition(t, `{"allOf":[{"type":"object","properties":{"selected":{"type":"boolean"}}},{"type":"object","properties":{"excluded":{"type":"string"}}}]}`)
	contract := s.matches[0].Endpoint.RequestContent.Representations[0].Schema
	contract.Raw = json.RawMessage(`{"$ref":"#/$defs/Node/allOf/0"}`)
	encoded, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "")
	// The shared pointer target is a subschema, not the definition's entire graph.
	if err != nil {
		t.Fatal(err)
	}
	document := decodeSharedOpenAPI(t, encoded)
	request := openAPITestOperationRequest(t, document, "node0")
	properties := request["properties"].(map[string]any)["input"].(map[string]any)["properties"].(map[string]any)
	// Selecting one allOf element must not import its sibling into the flat input.
	if properties["selected"] == nil || properties["excluded"] != nil {
		t.Fatal("shared subschema pointer lost its exact target")
	}
}

// TestAppOpenAPIBooleanSchemaRemainsFalse protects schema truth from the old
// object-only decoder that silently turned an authored false schema into {}.
func TestAppOpenAPIBooleanSchemaRemainsFalse(t *testing.T) {
	export := &appOpenAPIExport{components: make(map[string]any)}
	scope := &appOpenAPISchemaScope{export: export, namespace: "boolean_"}
	ref := scope.schema(&fusedobject.SchemaContract{Raw: json.RawMessage("false")})
	root := map[string]any{"components": map[string]any{"schemas": export.components}}
	value, found := schemaref.ResolveLocal(root, ref["$ref"].(string))
	// Boolean schema semantics must survive without projection fallback.
	if export.err != nil || !found || value != false {
		t.Fatal("false schema was weakened during export")
	}
}

// newSharedOpenAPIFixture builds several operations sharing one exact-version
// recursive schema dictionary without depending on an external Registry.
func newSharedOpenAPIFixture(t *testing.T, count int) (*appOpenAPISharedStore, *fusedobject.ServiceMetadata) {
	t.Helper()
	base, endpoint := newAppOpenAPIFixture(t)
	selections, err := decodeAppOpenAPISelections(base.app.Selections)
	// Fixture identity must remain pinned exactly as a production app selection.
	if err != nil {
		t.Fatal(err)
	}
	ref := store.ServiceContractMetadataRef{ServiceID: selections[0].ServiceID, ServiceVersionID: selections[0].ServiceVersionID}
	owner := &fusedobject.ServiceMetadata{ID: ref.ServiceID, ServiceVersionID: ref.ServiceVersionID, ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{RequiredCapabilities: []string{fusedobject.ExecutionCapabilityJSONSchemaSharedDefinitionsV1}}, SchemaDefinitions: map[string]fusedobject.SchemaContract{
		"Node":   sharedOpenAPIDefinition(t, `{"type":"object","properties":{"label":{"type":"string"},"child":{"$ref":"#/$defs/Node"},"counter":{"maximum":9007199254740993}},"examples":[{"$ref":"opaque-example"}]}`),
		"Unused": sharedOpenAPIDefinition(t, `{"description":"unused-provider-definition","type":"string"}`),
	}}
	endpoint.RequestContent = &fusedobject.RequestContent{Representations: []fusedobject.RequestRepresentation{{Schema: &fusedobject.SchemaContract{Raw: json.RawMessage(`{"$ref":"#/$defs/Node"}`), SharedDefinitions: true}}}}
	endpoint.Responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{Schema: endpoint.RequestContent.Representations[0].Schema}}}}
	base.matches = nil
	selections[0].EndpointIDs, selections[0].OperationNames = nil, nil
	for position := range count {
		copy := endpoint
		copy.ID, copy.Name = uuid.New(), "node"+string(rune('0'+position))
		base.matches = append(base.matches, store.ServiceContractEndpointMatch{SelectionIndex: 0, Endpoint: copy})
		selections[0].EndpointIDs = append(selections[0].EndpointIDs, copy.ID)
		selections[0].OperationNames = append(selections[0].OperationNames, copy.Name)
	}
	base.app.Selections, _ = json.Marshal(selections)
	return &appOpenAPISharedStore{appOpenAPITestStore: base, metadata: map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata{ref: owner}}, owner
}

// sharedOpenAPIDefinition uses the production canonical hash required when the
// fixture dictionary passes through shared snapshot admission.
func sharedOpenAPIDefinition(t *testing.T, raw string) fusedobject.SchemaContract {
	t.Helper()
	hash, err := canonicaljson.SHA256([]byte(raw))
	// Tests must not bypass schema integrity merely to construct a dictionary.
	if err != nil {
		t.Fatal(err)
	}
	return fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: json.RawMessage(raw), ContentHash: hex.EncodeToString(hash[:])}
}

// decodeSharedOpenAPI retains exact numbers while inspecting the actual output.
func decodeSharedOpenAPI(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var document map[string]any
	// Every assertion is against the complete produced document, not internal state.
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	return document
}

// countSharedOpenAPIDefinitions verifies serialization cardinality independent
// of the number of operation roots pointing at each version-owned definition.
func countSharedOpenAPIDefinitions(components map[string]any) int {
	count := 0
	for name := range components {
		// Root and local-subschema components are separate from shared dictionary entries.
		if strings.Contains(name, "_Definition_") {
			count++
		}
	}
	return count
}
