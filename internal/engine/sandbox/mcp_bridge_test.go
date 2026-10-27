package sandbox

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

type mcpBridgeFixture struct {
	mcpUnifiedAppFixture
	calls            int
	alias, transport string
}

// AttachedBridges supplies a scalar local contract to test the shared execute catalogue.
func (f *mcpBridgeFixture) AttachedBridges(context.Context, auth.RuntimeIdentity) ([]models.BridgeBinding, error) {
	return []models.BridgeBinding{{Alias: "volume", Kind: "command", InputSchema: json.RawMessage(`{"type":"integer"}`)}}, nil
}

// ExecuteAttachedBridge observes one transport dispatch without provider or broker side effects.
func (f *mcpBridgeFixture) ExecuteAttachedBridge(ctx context.Context, _ auth.RuntimeIdentity, alias string, input json.RawMessage) (json.RawMessage, error) {
	f.calls++
	f.alias = alias
	f.transport = ExecutionTransportFromContext(ctx)
	return input, nil
}

// TestBridgeMCPScalarAndOverrideBoundary verifies typed scalar calls and rejects injected routing controls.
func TestBridgeMCPScalarAndOverrideBoundary(t *testing.T) {
	fixture := &mcpBridgeFixture{}
	SetMCPCapabilityAdapter(fixture)
	defer SetMCPCapabilityAdapter(nil)
	id := uuid.New()
	admission := &mcpModernAdmission{target: &store.MCPRouteTarget{AppID: id}, identity: auth.RuntimeIdentity{AppID: id, Kind: store.AppKindMCP}, server: FixtureServerMetadata{Name: "local", Version: "1.0.0"}}
	tool := map[string]any{"name": "execute", "inputSchema": map[string]any{"type": "object"}}
	catalogue := map[string]any{"tools": []any{tool}}
	require.NoError(t, appendBridgeMCPTools(t.Context(), catalogue, admission))
	schema, err := json.Marshal(tool)
	require.NoError(t, err)
	require.Contains(t, string(schema), "bridge:volume")
	request := mcpJSONRPCRequest{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"name":"execute","arguments":{"operation":"bridge:volume","input":20}}`)}
	response := httptest.NewRecorder()
	require.True(t, handleBridgeMCPCall(t.Context(), response, httptest.NewRequest("POST", "/mcp", nil), request, admission))
	require.Equal(t, 200, response.Code)
	require.Equal(t, 1, fixture.calls)
	require.Equal(t, "volume", fixture.alias)
	require.Equal(t, "mcp", fixture.transport)
	require.Contains(t, response.Body.String(), `"result":20`)
	request.Params = json.RawMessage(`{"name":"execute","arguments":{"operation":"bridge:volume","input":20,"pairing_id":"foreign"}}`)
	response = httptest.NewRecorder()
	require.True(t, handleBridgeMCPCall(t.Context(), response, httptest.NewRequest("POST", "/mcp", nil), request, admission))
	require.Equal(t, 400, response.Code)
	require.Equal(t, 1, fixture.calls)
}
