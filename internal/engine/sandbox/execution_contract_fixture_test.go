package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type executionContractFixture struct {
	fusedobject.ExecutionContractEnvelope
	ServiceID        uuid.UUID       `json:"service_id"`
	ServiceVersionID uuid.UUID       `json:"service_version_id"`
	Version          string          `json:"version"`
	Service          json.RawMessage `json:"service"`
	Operations       json.RawMessage `json:"operations"`
	Webhooks         json.RawMessage `json:"webhooks"`
}

// TestExecutionContractV2GoldenFixtureRoundTripsSemantically ensures every accepted wire fixture can become an immutable Engine snapshot.
func TestExecutionContractV2GoldenFixtureRoundTripsSemantically(t *testing.T) {
	accepted := []string{
		"v2_full.json",
		"v2_absence.json",
		"v2_explicit_empty.json",
		"v2_explicit_false.json",
		"v2_multiple_alternatives.json",
		"v2_unknown_documentation_field.json",
		"v2_openapi32.json",
	}
	for _, name := range accepted {
		t.Run(name, func(t *testing.T) {
			payload := readExecutionContractFixture(t, name)
			assertExecutionContractFixtureRoundTrip(t, payload)
		})
	}

	payload := readExecutionContractFixture(t, "v2_full.json")
	var full runtimeContractBatchItem
	if err := json.Unmarshal(payload, &full); err != nil {
		t.Fatalf("decode full fixture into Engine runtime contract: %v", err)
	}
	requested := store.WorkspaceServiceVersion{
		ServiceID: full.ServiceID, ServiceVersionID: full.ServiceVersionID, Version: full.Version,
	}
	if _, err := runtimeContractSnapshotFromBatchItem(full, requested); err != nil {
		t.Fatalf("construct Engine snapshot from full fixture: %v", err)
	}

	assertOpenAPI32ExecutionFixture(t, readExecutionContractFixture(t, "v2_openapi32.json"))
}

// assertOpenAPI32ExecutionFixture proves the shared wire fixture can build the immutable Engine snapshot used for dispatch.
func assertOpenAPI32ExecutionFixture(t *testing.T, payload []byte) {
	t.Helper()
	var contract runtimeContractBatchItem
	if err := json.Unmarshal(payload, &contract); err != nil {
		t.Fatalf("decode OpenAPI 3.2 fixture into Engine runtime contract: %v", err)
	}
	requested := store.WorkspaceServiceVersion{
		ServiceID: contract.ServiceID, ServiceVersionID: contract.ServiceVersionID, Version: contract.Version,
	}
	snapshot, err := runtimeContractSnapshotFromBatchItem(contract, requested)
	if err != nil {
		t.Fatalf("construct Engine snapshot from OpenAPI 3.2 fixture: %v", err)
	}
	assertOpenAPI32Snapshot(t, snapshot)
}

// assertOpenAPI32Snapshot keeps transport, parameter, and media assertions together at the snapshot boundary.
func assertOpenAPI32Snapshot(t *testing.T, snapshot *store.ServiceContractSnapshot) {
	t.Helper()
	assertOpenAPI32ServiceFields(t, snapshot)
	assertOpenAPI32ParameterAndMethodFields(t, snapshot)
	assertOpenAPI32MediaFields(t, snapshot)
}

// assertOpenAPI32ServiceFields protects named-server and OAuth metadata required before request construction.
func assertOpenAPI32ServiceFields(t *testing.T, snapshot *store.ServiceContractSnapshot) {
	t.Helper()
	if len(snapshot.ServiceMetadata.Servers) != 1 || snapshot.ServiceMetadata.Servers[0].Name != "production" {
		t.Fatalf("OpenAPI 3.2 server mirror = %#v", snapshot.ServiceMetadata.Servers)
	}
	auth := snapshot.ServiceMetadata.AuthConfigs[0]
	if auth.OAuth2MetadataURL == "" || auth.Deprecated == nil || !*auth.Deprecated {
		t.Fatalf("OpenAPI 3.2 auth mirror = %#v", auth)
	}
}

// assertOpenAPI32ParameterAndMethodFields keeps zero-valued querystring controls distinct from inferred defaults.
func assertOpenAPI32ParameterAndMethodFields(t *testing.T, snapshot *store.ServiceContractSnapshot) {
	t.Helper()
	query := openAPI32Operation(t, snapshot.Endpoints, "search")
	serialization := query.Parameters[0].Serialization
	if serialization.Style != "" || serialization.Explode != nil || serialization.AllowReserved != nil || serialization.AllowEmptyValue != nil {
		t.Fatalf("querystring parameter serialization must remain zero-valued: %#v", serialization)
	}
	if len(query.Parameters[0].Content["application/x-www-form-urlencoded"].Encoding) != 1 {
		t.Fatalf("querystring property encoding mirror = %#v", query.Parameters[0].Content)
	}
	if openAPI32Operation(t, snapshot.Endpoints, "reportTunnel").Method != "RePoRt" {
		t.Fatal("custom method casing was not preserved")
	}
}

// assertOpenAPI32MediaFields ensures sequential and positional media reach Engine without a legacy flat representation.
func assertOpenAPI32MediaFields(t *testing.T, snapshot *store.ServiceContractSnapshot) {
	t.Helper()
	stream := openAPI32Operation(t, snapshot.Endpoints, "streamEvents")
	representation := stream.Responses["200"].Representations[0]
	if representation.MediaType != "text/event-stream" || representation.SSE == nil {
		t.Fatal("SSE response contract was not preserved")
	}
	upload := openAPI32Operation(t, snapshot.Endpoints, "uploadMedia")
	encoding := upload.RequestContent.Representations[0]
	if len(encoding.PrefixEncoding) != 3 || encoding.ItemEncoding == nil || len(encoding.PrefixEncoding[2].PrefixEncoding) != 1 {
		t.Fatalf("OpenAPI 3.2 positional mirror = %#v", encoding)
	}
}

// openAPI32Operation fails loudly on fixture drift instead of returning a zero operation that could mask missing projection data.
func openAPI32Operation(t *testing.T, operations []fusedobject.Endpoint, name string) *fusedobject.Endpoint {
	t.Helper()
	for index := range operations {
		if operations[index].Name == name {
			return &operations[index]
		}
	}
	t.Fatalf("OpenAPI 3.2 operation %q not found", name)
	return nil
}

func assertExecutionContractFixtureRoundTrip(t *testing.T, payload []byte) {
	t.Helper()
	var fixture executionContractFixture
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatalf("decode fixture envelope: %v", err)
	}
	if err := fusedobject.ValidateExecutionContractEnvelope(fixture.ExecutionContractEnvelope); err != nil {
		t.Fatalf("validate fixture envelope: %v", err)
	}
	reencoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-encode fixture: %v", err)
	}
	assertSemanticJSONEqual(t, payload, reencoded)

	var item runtimeContractBatchItem
	if err := json.Unmarshal(payload, &item); err != nil {
		t.Fatalf("decode fixture into Engine runtime contract: %v", err)
	}
}

func TestExecutionContractGoldenFixtureRejectsUnknownCapability(t *testing.T) {
	payload := readExecutionContractFixture(t, "v2_unknown_required_capability.json")
	var item runtimeContractBatchItem
	if err := json.Unmarshal(payload, &item); err != nil {
		t.Fatalf("decode rejection fixture: %v", err)
	}
	err := validateRuntimeContractBatchEnvelopes([]runtimeContractBatchItem{item})
	if err == nil || !strings.Contains(err.Error(), fusedobject.ExecutionCapabilityRequiredCode) {
		t.Fatalf("validate rejection fixture error = %v", err)
	}
}

func readExecutionContractFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "contract-fixtures", "execution", name)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return payload
}

func assertSemanticJSONEqual(t *testing.T, expected, actual []byte) {
	t.Helper()
	var expectedValue, actualValue any
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatalf("decode actual JSON: %v", err)
	}
	if !reflect.DeepEqual(expectedValue, actualValue) {
		t.Fatalf("semantic fixture mismatch\nexpected: %s\nactual: %s", expected, actual)
	}
}
