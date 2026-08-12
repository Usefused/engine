package sandbox

import (
	"encoding/json"
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

// TestExecutionContractV2RoundTripsSemantically proves the
// standalone Engine can persist its canonical envelope without a Registry repo.
func TestExecutionContractV2RoundTripsSemantically(t *testing.T) {
	item := localRuntimeContractItem()
	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode local runtime contract: %v", err)
	}
	assertExecutionContractFixtureRoundTrip(t, payload)
	requested := store.WorkspaceServiceVersion{
		ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID, Version: item.Version,
	}
	if _, err := runtimeContractSnapshotFromBatchItem(item, requested); err != nil {
		t.Fatalf("construct Engine snapshot: %v", err)
	}
}

// TestExecutionContractRejectsUnknownCapability keeps capability
// negotiation fail-closed without retaining an external rejection fixture.
func TestExecutionContractRejectsUnknownCapability(t *testing.T) {
	item := localRuntimeContractItem()
	item.RequiredCapabilities = []string{"http.future.v1"}
	err := validateRuntimeContractBatchEnvelopes([]runtimeContractBatchItem{item})
	if err == nil || !strings.Contains(err.Error(), fusedobject.ExecutionCapabilityRequiredCode) {
		t.Fatalf("validate rejection contract error = %v", err)
	}
}

// localRuntimeContractItem uses stable identities so JSON round-trip failures
// are reproducible while remaining fully owned by the Engine repository.
func localRuntimeContractItem() runtimeContractBatchItem {
	serviceID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	versionID := uuid.MustParse("20000000-0000-4000-8000-000000000002")
	return runtimeContractBatchItem{
		ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{
			ContractVersion: fusedobject.CurrentExecutionContractVersion, RequiredCapabilities: []string{},
		},
		ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1",
		Service:    &runtimeContractService{ID: serviceID, Name: "Local Contract", BaseURL: "https://api.example.test"},
		Operations: []fusedobject.Endpoint{}, Webhooks: []fusedobject.Webhook{},
	}
}

// assertExecutionContractFixtureRoundTrip verifies both the wire envelope and
// typed DTO preserve explicit empty arrays used by capability negotiation.
func assertExecutionContractFixtureRoundTrip(t *testing.T, payload []byte) {
	t.Helper()
	var fixture executionContractFixture
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatalf("decode contract envelope: %v", err)
	}
	if err := fusedobject.ValidateExecutionContractEnvelope(fixture.ExecutionContractEnvelope); err != nil {
		t.Fatalf("validate contract envelope: %v", err)
	}
	reencoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-encode contract envelope: %v", err)
	}
	assertSemanticJSONEqual(t, payload, reencoded)
	var item runtimeContractBatchItem
	if err := json.Unmarshal(payload, &item); err != nil {
		t.Fatalf("decode contract into Engine DTO: %v", err)
	}
}

// assertSemanticJSONEqual compares decoded values because field order is not a
// wire guarantee and should not make Engine unit tests brittle.
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
		t.Fatalf("semantic contract mismatch\nexpected: %s\nactual: %s", expected, actual)
	}
}
