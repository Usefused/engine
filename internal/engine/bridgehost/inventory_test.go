package bridgehost

import (
	"encoding/json"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/stretchr/testify/require"
	"sort"
	"testing"
)

// inventoryFixture uses the same compact sorted JSON digest as the Bridge client.
func inventoryFixture(t *testing.T, profiles ...string) *bridgev1.Inventory {
	t.Helper()
	ids := append([]string{}, profiles...)
	sort.Strings(ids)
	raw, err := json.Marshal(ids)
	require.NoError(t, err)
	return &bridgev1.Inventory{ProfileIds: ids, Hash: Digest(raw)}
}

// TestInventoryValidation rejects ambiguous or tampered snapshots, including an incorrectly hashed empty workspace.
func TestInventoryValidation(t *testing.T) {
	_, err := validateInventory(inventoryFixture(t))
	require.NoError(t, err)
	_, err = validateInventory(inventoryFixture(t, "same", "same"))
	require.ErrorIs(t, err, ErrContract)
	_, err = validateInventory(inventoryFixture(t, "bad/id"))
	require.ErrorIs(t, err, ErrContract)
	frame := inventoryFixture(t, "one")
	frame.ProfileIds = []string{"two"}
	_, err = validateInventory(frame)
	require.ErrorIs(t, err, ErrContract)
}

// TestWorkspaceInventoryReconcilesAcrossReconnect makes unseen deletions authoritative without losing retained schemas.
func TestWorkspaceInventoryReconcilesAcrossReconnect(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, _ := pairingFixture(t, repo, actor)
	manifest := eventManifest(t, "1", false)
	require.NoError(t, repo.PutManifest(t.Context(), p, manifest))
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "sensors", "new")))
	var state string
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT state FROM fused_bridge_profiles WHERE pairing_id=$1 AND profile_id='new'`, p.ID).Scan(&state))
	require.Equal(t, "offline", state)
	generation, err := repo.Claim(t.Context(), p, "replacement")
	require.NoError(t, err)
	p.Generation = generation
	var document ManifestDocument
	require.NoError(t, json.Unmarshal(manifest.ManifestJson, &document))
	pin := Pin{PairingID: p.ID, ProfileID: "sensors", ManifestHash: manifest.Hash, ConnectionID: "mqtt", OperationID: "messages", ContractHash: document.Operations[0].ContractHash}
	// A replacement generation cannot drain stale subscriptions before reconciling its workspace.
	require.ErrorIs(t, New(repo.Pool, nil).AuthorizeEventPin(t.Context(), p.AccountID, pin), ErrDenied)
	require.ErrorIs(t, repo.PutManifest(t.Context(), p, manifest), ErrDenied)
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t)))
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT state FROM fused_bridge_profiles WHERE pairing_id=$1 AND profile_id='sensors'`, p.ID).Scan(&state))
	require.Equal(t, "removed", state)
	manifest.Historical = true
	require.ErrorIs(t, repo.PutManifest(t.Context(), p, manifest), ErrDenied)
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "sensors")))
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT state FROM fused_bridge_profiles WHERE pairing_id=$1 AND profile_id='sensors'`, p.ID).Scan(&state))
	require.Equal(t, "offline", state)
}

// TestLegacyPairingRequiresWorkspaceConsent prevents schema migration from silently widening old grants.
func TestLegacyPairingRequiresWorkspaceConsent(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, secret := pairingFixture(t, repo, actor)
	_, err := repo.Pool.Exec(t.Context(), `UPDATE fused_bridge_pairings SET scope=DEFAULT WHERE id=$1`, p.ID)
	require.NoError(t, err)
	_, err = repo.Authenticate(t.Context(), secret)
	require.ErrorIs(t, err, ErrDenied)
	require.ErrorIs(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "new")), ErrDenied)
	require.ErrorIs(t, repo.Renew(t.Context(), p.ID, p.Generation), ErrDenied)
}

// TestOfflineManifestRetainsCatalogue preserves known schemas while disabling all live command dispatch.
func TestOfflineManifestRetainsCatalogue(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, _ := pairingFixture(t, repo, actor)
	manifest := eventManifest(t, "1", false)
	require.NoError(t, repo.PutManifest(t.Context(), p, manifest))
	doc := ManifestDocument{Version: 1, ProfileID: "sensors", Revision: "2", State: "offline", Operations: []Operation{}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	offline := &bridgev1.Manifest{ProfileId: doc.ProfileID, Revision: doc.Revision, Hash: Digest(raw), ManifestJson: raw}
	require.NoError(t, repo.PutManifest(t.Context(), p, offline))
	var state, hash string
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT state,manifest_hash FROM fused_bridge_profiles WHERE pairing_id=$1 AND profile_id='sensors'`, p.ID).Scan(&state, &hash))
	require.Equal(t, "offline", state)
	require.Equal(t, manifest.Hash, hash)
}
