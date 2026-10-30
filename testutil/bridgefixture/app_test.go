package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/capability"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestPublishConsumerFixturePreservesBridgeCapability checks the same immutable publication boundary used by real app execution.
func TestPublishConsumerFixturePreservesBridgeCapability(t *testing.T) {
	dsn := os.Getenv("BRIDGE_TEST_DATABASE_URL")
	// Fixture tests must never discover or mutate the user's running Engine database.
	if dsn == "" {
		t.Skip("BRIDGE_TEST_DATABASE_URL must name a disposable test database")
	}
	ctx := t.Context()
	pool, cleanup, err := openFixtureDatabase(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	repository := store.NewPostgresStore(pool)
	actor, _, err := fixtureActor(ctx, repository)
	require.NoError(t, err)
	bridges := &bridgehost.Repository{Pool: pool}
	pairing, _, err := bridges.Create(ctx, actor, uuid.NewString(), "Fixture publication test", "workspace")
	require.NoError(t, err)
	pairing.Generation, err = bridges.Claim(ctx, pairing, "fixture")
	require.NoError(t, err)
	require.NoError(t, bridges.PutInventory(ctx, pairing, &bridgev1.Inventory{ProfileIds: []string{"fixture"}, Hash: bridgehost.Digest([]byte(`["fixture"]`))}))
	contract := `{"kind":"read","input_schema":{"type":"object"},"output_schema":{"type":"integer"}}`
	document := bridgehost.ManifestDocument{Version: 1, ProfileID: "fixture", Revision: "1", Operations: []bridgehost.Operation{{ConnectionID: "demo", OperationID: "read", ContractHash: bridgehost.Digest([]byte(contract)), ContractJSON: contract}}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	manifest := &bridgev1.Manifest{ProfileId: document.ProfileID, Revision: document.Revision, Hash: bridgehost.Digest(raw), ManifestJson: raw}
	require.NoError(t, bridges.PutManifest(ctx, pairing, manifest))
	pin := bridgehost.Pin{PairingID: pairing.ID, ProfileID: document.ProfileID, ManifestHash: manifest.Hash, ConnectionID: "demo", OperationID: "read", ContractHash: document.Operations[0].ContractHash}
	appID, _, err := publishConsumerFixture(ctx, pool, repository, actor, pin)
	require.NoError(t, err)
	// Runtime must accept the fixture publication without weakening capability/binding consistency checks.
	bindings, err := repository.(store.BridgeBindingStore).ReadBridgeBindings(ctx, actor.AccountID, appID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.Equal(t, "fixture", bindings[0].Alias)
	require.Equal(t, pin.PairingID, bindings[0].PairingID)
	require.Equal(t, pin.ContractHash, bindings[0].ContractHash)
	var capabilities []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT array_agg(capability_key ORDER BY capability_key) FROM fused_app_capabilities WHERE app_id=$1`, appID).Scan(&capabilities))
	expectedKeys, expectedHash, err := capability.KeysAndHashWithBridges([]byte(`[]`), bindings)
	require.NoError(t, err)
	require.Equal(t, expectedKeys, capabilities)
	app, err := repository.GetApp(ctx, appID)
	require.NoError(t, err)
	require.Equal(t, expectedHash, app.CapabilityHash)
}
