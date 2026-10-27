package bridgehost

import (
	"encoding/json"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"testing"
)

// manifestFixture supplies exact contract bytes shared by both sides of the protocol.
func manifestFixture(t *testing.T) *bridgev1.Manifest {
	t.Helper()
	contract := `{"kind":"read","input_schema":{"type":"object","additionalProperties":false},"output_schema":{"type":"integer"}}`
	doc := ManifestDocument{Version: 1, ProfileID: "sensors", Revision: "1", Operations: []Operation{{ConnectionID: "mqtt", OperationID: "status", ContractHash: Digest([]byte(contract)), ContractJSON: contract}}}
	raw, err := json.Marshal(doc)
	// Fixture failures must stop before assertions can pass accidentally.
	if err != nil {
		t.Fatal(err)
	}
	return &bridgev1.Manifest{ProfileId: "sensors", Revision: "1", Hash: Digest(raw), ManifestJson: raw}
}

// TestManifestTamperingCannotChangePinnedOperations exercises the immutable contract boundary.
func TestManifestTamperingCannotChangePinnedOperations(t *testing.T) {
	f := manifestFixture(t)
	// A valid manifest establishes the positive control for tampering checks.
	if _, err := ValidateManifest(f); err != nil {
		t.Fatal(err)
	}
	f.ManifestJson = append(f.ManifestJson, ' ')
	// Even semantically identical but changed bytes require a new hash.
	if _, err := ValidateManifest(f); err == nil {
		t.Fatal("changed bytes accepted")
	}
	f = manifestFixture(t)
	f.Revision = "2"
	// Frame metadata cannot redirect an existing manifest to another revision.
	if _, err := ValidateManifest(f); err == nil {
		t.Fatal("revision mismatch accepted")
	}
}

// TestPayloadValidationPreservesRootShape prevents transport coercion from weakening schemas.
func TestPayloadValidationPreservesRootShape(t *testing.T) {
	// Scalar outputs remain valid without wrapping into an object.
	if err := ValidatePayload([]byte(`{"type":"integer"}`), []byte(`42`)); err != nil {
		t.Fatal(err)
	}
	// An object cannot masquerade as the declared scalar output.
	if err := ValidatePayload([]byte(`{"type":"integer"}`), []byte(`{"value":42}`)); err == nil {
		t.Fatal("invalid root accepted")
	}
}

// TestEmptyManifestWithdrawsCapabilities allows a stopped profile to remove stale current operations.
func TestEmptyManifestWithdrawsCapabilities(t *testing.T) {
	raw := []byte(`{"version":1,"profile_id":"sensors","revision":"empty","operations":[]}`)
	frame := &bridgev1.Manifest{ProfileId: "sensors", Revision: "empty", Hash: Digest(raw), ManifestJson: raw}
	// Withdrawal snapshots retain identity validation while granting no executable operations.
	if _, err := ValidateManifest(frame); err != nil {
		t.Fatal(err)
	}
}
