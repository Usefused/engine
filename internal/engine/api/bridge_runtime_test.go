package api

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

type bridgeRuntimeFixture struct {
	store.Store
	bindings     []models.BridgeBinding
	reads, calls int
	pin          bridgehost.Pin
}

// ResolveBridgeBindings satisfies plan storage without broadening runtime test authority.
func (f *bridgeRuntimeFixture) ResolveBridgeBindings(context.Context, uuid.UUID, map[string]models.BridgeReference) ([]models.BridgeBinding, error) {
	return f.bindings, nil
}

// ReadBridgeBindings observes whether denied tokens reach immutable app storage.
func (f *bridgeRuntimeFixture) ReadBridgeBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.BridgeBinding, error) {
	f.reads++
	return f.bindings, nil
}

// Invoke records the exact app-selected pin and preserves scalar JSON results.
func (f *bridgeRuntimeFixture) Invoke(_ context.Context, _ string, pin bridgehost.Pin, input []byte) ([]byte, error) {
	f.calls++
	f.pin = pin
	return input, nil
}

// TestBridgeRuntimeAuthority exercises token denial, alias membership, event exclusion and exact pin dispatch.
func TestBridgeRuntimeAuthority(t *testing.T) {
	binding := models.BridgeBinding{Alias: "volume", Kind: "command", ContractHash: "sha256:contract", BridgeReference: models.BridgeReference{PairingID: uuid.NewString(), ProfileID: "local", ManifestHash: "sha256:manifest", ConnectionID: "speaker", OperationID: "set"}}
	f := &bridgeRuntimeFixture{bindings: []models.BridgeBinding{binding}}
	s := &EngineGRPCServer{store: f, bridgeHost: f}
	identity := auth.RuntimeIdentity{Kind: store.AppKindSDK, AppID: uuid.New(), TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"unrelated"}}}
	_, err := s.ExecuteAttachedBridge(t.Context(), identity, "volume", json.RawMessage(`20`))
	require.Error(t, err)
	require.Zero(t, f.reads)
	require.Zero(t, f.calls)
	identity.TokenPolicy = store.AppTokenPolicy{AllowedOperations: []string{"bridge:volume", "bridge:missing"}}
	_, err = s.ExecuteAttachedBridge(t.Context(), identity, "missing", json.RawMessage(`20`))
	require.Error(t, err)
	require.Zero(t, f.calls)
	result, err := s.ExecuteAttachedBridge(t.Context(), identity, "volume", json.RawMessage(`20`))
	require.NoError(t, err)
	require.JSONEq(t, `20`, string(result))
	require.Equal(t, 1, f.calls)
	require.Equal(t, binding.ManifestHash, f.pin.ManifestHash)
	require.Equal(t, binding.ContractHash, f.pin.ContractHash)
	f.bindings[0].Kind = "event"
	_, err = s.ExecuteAttachedBridge(t.Context(), identity, "volume", json.RawMessage(`20`))
	require.Error(t, err)
	require.Equal(t, 1, f.calls)
}

// TestBridgeAutomaticSnapshotRequiresRunIdentity rejects untracked effects before dispatch.
func TestBridgeAutomaticSnapshotRequiresRunIdentity(t *testing.T) {
	host := &executionCapabilityHost{automaticSnapshot: true, apps: &EngineGRPCServer{}}
	_, err := host.callBridge(t.Context(), json.RawMessage(`{"bridge":"volume","input":20}`))
	require.Error(t, err)
	require.False(t, host.called)
	require.Zero(t, host.callCount)
}

// AuthorizeEventPin records that event subscriptions recheck the live installation separately from app membership.
func (f *bridgeRuntimeFixture) AuthorizeEventPin(_ context.Context, _ string, pin bridgehost.Pin) error {
	f.calls++
	f.pin = pin
	return nil
}

// TestBridgeEventSelectionUsesPinnedContract rejects ungranted event subscriptions before broker filtering.
func TestBridgeEventSelectionUsesPinnedContract(t *testing.T) {
	binding := models.BridgeBinding{Alias: "readings", Kind: "event", ContractHash: "sha256:event", BridgeReference: models.BridgeReference{PairingID: uuid.NewString(), ProfileID: "local", ManifestHash: "sha256:manifest", ConnectionID: "sensor", OperationID: "readings"}}
	f := &bridgeRuntimeFixture{bindings: []models.BridgeBinding{binding}}
	s := &EngineGRPCServer{store: f, bridgeHost: f}
	identity := auth.RuntimeIdentity{Kind: store.AppKindSDK, TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"bridge:readings"}}}
	pin := bridgehost.Pin{PairingID: binding.PairingID, ProfileID: binding.ProfileID, ManifestHash: binding.ManifestHash, ConnectionID: binding.ConnectionID, OperationID: binding.OperationID, ContractHash: binding.ContractHash}
	key := binding.PairingID + "." + bridgehost.EventName(pin)
	subjects, err := s.bridgeEventSubjects(t.Context(), identity, []string{key})
	require.NoError(t, err)
	require.Equal(t, []string{bridgehost.EventSubject(identity.AccountID.String(), pin)}, subjects)
	require.Equal(t, 1, f.calls)
	identity.TokenPolicy = store.AppTokenPolicy{AllowedOperations: []string{"bridge:other"}}
	_, err = s.bridgeEventSubjects(t.Context(), identity, []string{key})
	require.Error(t, err)
	require.Equal(t, 1, f.calls)
}

// TestBridgeAdapterAdmission rejects event selections until the adapter has a durable receiver.
func TestBridgeAdapterAdmission(t *testing.T) {
	event := []models.BridgeBinding{{Alias: "readings", Kind: "event"}}
	_, err := validateBridgeAdapterKinds("sdk", event)
	require.NoError(t, err)
	_, err = validateBridgeAdapterKinds("mcp", event)
	require.Error(t, err)
	_, err = validateBridgeAdapterKinds("unified_app", event)
	require.Error(t, err)
	_, err = validateBridgeAdapterKinds("unified_app", []models.BridgeBinding{{Alias: "set", Kind: "command"}})
	require.NoError(t, err)
}

// TestBridgeWorkspaceAdmission prevents account-wide pairing lookup from widening workspace authority.
func TestBridgeWorkspaceAdmission(t *testing.T) {
	workspace := uuid.New()
	require.NoError(t, validateBridgeWorkspace(workspace, []models.BridgeBinding{{WorkspaceID: workspace.String()}}))
	require.Error(t, validateBridgeWorkspace(workspace, []models.BridgeBinding{{WorkspaceID: uuid.NewString()}}))
	require.Error(t, validateBridgeWorkspace(workspace, []models.BridgeBinding{{}}))
}

// TestBridgeRESTResponseMatchesSharedEnvelope exercises authenticated routing, exact app membership and typed JSON preservation.
func TestBridgeRESTResponseMatchesSharedEnvelope(t *testing.T) {
	server, appID := newRESTPhysicalServer(&restRuntimeTestDouble{})
	original := server.store.(*grpcRuntimeStore)
	original.scope.Selections = []byte(`[]`)
	fixture := &bridgeRuntimeFixture{Store: server.store, bindings: []models.BridgeBinding{{Alias: "echo_device", Kind: "read", BridgeReference: models.BridgeReference{PairingID: uuid.NewString(), ProfileID: "echo-live", ConnectionID: "speaker", OperationID: "device.info"}}}}
	server.store, server.bridgeHost = fixture, fixture
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"bridge:echo_device","input":{"online":true}}`, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var envelope struct {
		AppID      string            `json:"app_id"`
		Operation  string            `json:"operation"`
		Kind       string            `json:"kind"`
		StatusCode int               `json:"status_code"`
		Results    []json.RawMessage `json:"results"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Equal(t, appID.String(), envelope.AppID)
	require.Equal(t, "bridge:echo_device", envelope.Operation)
	require.Equal(t, "bridge", envelope.Kind)
	require.Equal(t, http.StatusOK, envelope.StatusCode)
	require.Len(t, envelope.Results, 1)
	require.JSONEq(t, `{"online":true}`, string(envelope.Results[0]))
	require.Equal(t, 1, fixture.calls)
}
