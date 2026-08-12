package sandbox

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

type sharedExecutionFixtureManifest struct {
	ManifestVersion int                      `json:"manifest_version"`
	Fixtures        []sharedExecutionFixture `json:"fixtures"`
}

type sharedExecutionFixture struct {
	File    string `json:"file"`
	Outcome string `json:"outcome"`
}

func TestSharedExecutionFixtureManifestRoundTripsThroughEngineDTO(t *testing.T) {
	manifest := readEngineExecutionFixtureManifest(t)
	for _, fixture := range manifest.Fixtures {
		fixture := fixture
		t.Run(fixture.File, func(t *testing.T) {
			payload := readExecutionContractFixture(t, fixture.File)
			assertEngineWireRoundTrip(t, payload)
			var item runtimeContractBatchItem
			if err := json.Unmarshal(payload, &item); err != nil {
				t.Fatalf("decode Engine DTO: %v", err)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("re-encode Engine DTO: %v", err)
			}
			assertEngineTypedReencodeStable(t, encoded)
			assertEngineFixtureOutcome(t, item, fixture.Outcome)
		})
	}
}

func assertEngineWireRoundTrip(t *testing.T, payload []byte) {
	t.Helper()
	var wire executionContractFixture
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("decode Engine wire fixture: %v", err)
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("re-encode Engine wire fixture: %v", err)
	}
	assertSemanticJSONEqual(t, payload, encoded)
}

func assertEngineTypedReencodeStable(t *testing.T, encoded []byte) {
	t.Helper()
	var decoded runtimeContractBatchItem
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode canonical Engine DTO: %v", err)
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode canonical Engine DTO: %v", err)
	}
	assertSemanticJSONEqual(t, encoded, reencoded)
}

func readEngineExecutionFixtureManifest(t *testing.T) sharedExecutionFixtureManifest {
	t.Helper()
	payload := readExecutionContractFixture(t, "execution-fixture-manifest.json")
	var manifest sharedExecutionFixtureManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode execution fixture manifest: %v", err)
	}
	if manifest.ManifestVersion != 1 || len(manifest.Fixtures) == 0 {
		t.Fatalf("invalid execution fixture manifest: %#v", manifest)
	}
	return manifest
}

func assertEngineFixtureOutcome(t *testing.T, item runtimeContractBatchItem, outcome string) {
	t.Helper()
	err := fusedobject.ValidateExecutionContractEnvelope(item.ExecutionContractEnvelope)
	if outcome == "accepted" && err == nil {
		return
	}
	compatibilityErr, classified := fusedobject.ExecutionContractCompatibilityDetails(err)
	if outcome == fusedobject.ExecutionCapabilityRequiredCode && classified && compatibilityErr.Reason == fusedobject.ExecutionContractReasonUnsupportedCapability {
		return
	}
	var typedErr *fusedobject.ExecutionContractCompatibilityError
	t.Fatalf("fixture outcome = %q, validation error = %#v, typed=%t", outcome, err, errors.As(err, &typedErr))
}
