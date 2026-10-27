package capability

import (
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/stretchr/testify/require"
	"testing"
)

// TestBridgeCapabilityPinsChangeFamilyAuthority prevents a stable alias from concealing a changed destination or contract.
func TestBridgeCapabilityPinsChangeFamilyAuthority(t *testing.T) {
	b := models.BridgeBinding{Alias: "volume", ContractHash: "sha256:one", BridgeReference: models.BridgeReference{PairingID: "pair", ProfileID: "profile", ManifestHash: "sha256:manifest", ConnectionID: "speaker", OperationID: "set"}}
	keys, hash, err := KeysAndHashWithBridges([]byte(`[]`), []models.BridgeBinding{b})
	require.NoError(t, err)
	require.Len(t, keys, 1)
	b.ContractHash = "sha256:two"
	_, changed, err := KeysAndHashWithBridges([]byte(`[]`), []models.BridgeBinding{b})
	require.NoError(t, err)
	require.NotEqual(t, hash, changed)
}

// TestBridgeCapabilityIdentityDelimiters cannot confuse distinct connection and operation identities containing colons.
func TestBridgeCapabilityIdentityDelimiters(t *testing.T) {
	first := models.BridgeBinding{Alias: "selected", BridgeReference: models.BridgeReference{PairingID: "pair", ProfileID: "profile", ManifestHash: "manifest", ConnectionID: "a:b", OperationID: "c"}}
	second := first
	second.ConnectionID = "a"
	second.OperationID = "b:c"
	_, oneHash, err := KeysAndHashWithBridges([]byte(`[]`), []models.BridgeBinding{first})
	require.NoError(t, err)
	_, twoHash, err := KeysAndHashWithBridges([]byte(`[]`), []models.BridgeBinding{second})
	require.NoError(t, err)
	require.NotEqual(t, oneHash, twoHash)
}

// TestBridgeMetadataCannotHidePhysicalCapabilities keeps untrusted selection JSON separate from resolved local bindings.
func TestBridgeMetadataCannotHidePhysicalCapabilities(t *testing.T) {
	raw := []byte(`[{"service_id":"11111111-1111-4111-8111-111111111111","service_version_id":"22222222-2222-4222-8222-222222222222","operation_names":["read"],"bridge":{"alias":"fake"}}]`)
	keys, _, err := KeysAndHashWithBridges(raw, nil)
	require.NoError(t, err)
	require.Contains(t, keys, "service:11111111-1111-4111-8111-111111111111:22222222-2222-4222-8222-222222222222:operation:read")
}
