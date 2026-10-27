package store

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/shared/capability"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

// TestBridgeOnlyApplyPublishesRuntimeAndToken exercises real PostgreSQL publication and token authorization together.
func TestBridgeOnlyApplyPublishesRuntimeAndToken(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	binding := models.BridgeBinding{Alias: "echo_device", Kind: "read", WorkspaceID: uuid.NewString(),
		BridgeReference: models.BridgeReference{PairingID: uuid.NewString(), ProfileID: "echo-live", ConnectionID: "speaker", OperationID: "device.info", ManifestHash: "sha256:manifest"}, ContractHash: "sha256:contract", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"boolean"}`)}
	fixture.params.Scope.Selections = []byte(`[]`)
	fixture.params.Scope.Bridges = []models.BridgeBinding{binding}
	fixture.params.SDKGenerationJobID = ""
	fixture.params.SDKGenerationStatus = models.SDKGenerationStatusSkipped
	fixture.params.TokenHash = "bridge-apply-test-" + uuid.NewString()
	payload, err := json.Marshal(map[string]any{"bridges": fixture.params.Scope.Bridges, "description": fixture.params.Scope.Description})
	require.NoError(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `UPDATE fused_config_plans SET resolved_payload=$2 WHERE id=$1`, fixture.params.Plan.PlanID, payload)
	require.NoError(t, err)
	result, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	require.NoError(t, err)
	require.True(t, result.VersionCreated)
	require.True(t, result.TokenCreated)
	persisted := NewPostgresStore(fixture.pool).(*postgresStore)
	app, err := persisted.GetApp(fixture.ctx, result.AppID)
	require.NoError(t, err)
	keys, hash, err := capability.KeysAndHashWithBridges(fixture.params.Scope.Selections, fixture.params.Scope.Bridges)
	require.NoError(t, err)
	var persistedKeys []string
	require.NoError(t, fixture.pool.QueryRow(fixture.ctx, `SELECT array_agg(capability_key ORDER BY capability_key) FROM fused_app_capabilities WHERE app_id=$1`, result.AppID).Scan(&persistedKeys))
	require.Equal(t, keys, persistedKeys)
	require.Equal(t, hash, app.CapabilityHash)
	projection, err := persisted.AuthorizeApp(fixture.ctx, result.AppID, fixture.params.TokenHash)
	require.NoError(t, err)
	require.NotNil(t, projection)
	bindings, err := persisted.ReadBridgeBindings(fixture.ctx, fixture.params.Scope.AccountID, result.AppID)
	require.NoError(t, err)
	expectedBindings, err := json.Marshal(fixture.params.Scope.Bridges)
	require.NoError(t, err)
	storedBindings, err := json.Marshal(bindings)
	require.NoError(t, err)
	require.JSONEq(t, string(expectedBindings), string(storedBindings))
	plan, err := fixture.repository.GetConfigPlan(fixture.ctx, fixture.params.Plan.PlanID)
	require.NoError(t, err)
	require.Equal(t, ConfigPlanStatusApplied, plan.Status)
}
