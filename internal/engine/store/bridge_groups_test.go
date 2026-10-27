package store

import (
	"encoding/json"
	"fmt"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/stretchr/testify/require"
	"testing"
)

func groupFixture(operations string) (models.BridgeReference, bridgeGroupCatalogue) {
	ref := models.BridgeReference{Bridge: "home-bridge", Profile: "echo-live", Connections: map[string]models.BridgeConnectionSelection{"echo-show-8": {Operations: json.RawMessage(operations)}}}
	return ref, bridgeGroupCatalogue{Pairing: "paired", Hash: "pinned", Document: []byte(`{"operations":[{"connection_id":"echo-show-8","operation_id":"device.info","contract_json":"{\"kind\":\"read\"}"},{"connection_id":"echo-show-8","operation_id":"volume.set","contract_json":"{\"kind\":\"command\"}"},{"connection_id":"echo-show-8","operation_id":"player.changed","contract_json":"{\"kind\":\"event\"}"},{"connection_id":"other","operation_id":"device.info","contract_json":"{\"kind\":\"read\"}"}]}`)}
}

func TestBridgeGroupExpansionPinsOnlySelectedConnection(t *testing.T) {
	ref, c := groupFixture(`"*"`)
	pins, err := expandBridgeGroup("home", ref, c)
	require.NoError(t, err)
	require.Len(t, pins, 2)
	for _, pin := range pins {
		require.Equal(t, "echo-show-8", pin.ConnectionID)
		require.Equal(t, "pinned", pin.ManifestHash)
		require.NotEqual(t, "player.changed", pin.OperationID)
	}
	ref.Connections["echo-show-8"] = models.BridgeConnectionSelection{Operations: json.RawMessage(`["device.info","volume.set"]`)}
	explicit, err := expandBridgeGroup("home", ref, c)
	require.NoError(t, err)
	require.Equal(t, pins, explicit)
	ref.Connections["other"] = models.BridgeConnectionSelection{Operations: json.RawMessage(`"*"`)}
	both, err := expandBridgeGroup("home", ref, c)
	require.NoError(t, err)
	require.Len(t, both, 3)
	require.NotEqual(t, bridgeGroupAlias("home", "echo-show-8", "device.info"), bridgeGroupAlias("home", "other", "device.info"))
}

func TestBridgeGroupRejectsMissingEventsAndAmbiguousPaths(t *testing.T) {
	for _, ops := range []string{`[]`, `["absent"]`, `["player.changed"]`, `["device.info","device.info"]`, `["*"]`, `null`} {
		ref, c := groupFixture(ops)
		_, err := expandBridgeGroup("home", ref, c)
		require.Error(t, err, ops)
	}
	ref, c := groupFixture(`"*"`)
	ref.Connections = map[string]models.BridgeConnectionSelection{"missing": {Operations: json.RawMessage(`"*"`)}}
	_, err := expandBridgeGroup("home", ref, c)
	require.ErrorContains(t, err, "connection")
	ref, c = groupFixture(`"*"`)
	var ops []map[string]string
	for i := 0; i < 101; i++ {
		ops = append(ops, map[string]string{"connection_id": "echo-show-8", "operation_id": fmt.Sprintf("op%d", i), "contract_json": `{"kind":"read"}`})
	}
	c.Document, _ = json.Marshal(map[string]any{"operations": ops})
	_, err = expandBridgeGroup("home", ref, c)
	require.ErrorContains(t, err, "100")
}
