package sandbox

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type mcpExecutionAppFixture struct {
	listed   int
	executed int
	input    json.RawMessage
}

// ExecutionAppInputSchema returns one exact version's authored input schema.
func (fixture *mcpExecutionAppFixture) ExecutionAppInputSchema(context.Context, auth.RuntimeIdentity) (json.RawMessage, bool, error) {
	fixture.listed++
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`), true, nil
}

// ExecuteExecutionApp records the selected input and returns one shared execution envelope.
func (fixture *mcpExecutionAppFixture) ExecuteExecutionApp(_ context.Context, _ auth.RuntimeIdentity, input json.RawMessage) (json.RawMessage, error) {
	fixture.executed++
	fixture.input = append(json.RawMessage(nil), input...)
	return json.RawMessage(`{"executionId":"11111111-1111-4111-8111-111111111111","status":"succeeded","output":{"customerId":"cus_123"}}`), nil
}

// TestMCPExecuteToolSharesExecutionAppCommand checks the existing execute tool's authored variant.
func TestMCPExecuteToolSharesExecutionAppCommand(t *testing.T) {
	fixture := &mcpExecutionAppFixture{}
	SetMCPCapabilityAdapter(fixture)
	defer SetMCPCapabilityAdapter(nil)
	appID := uuid.New()
	admission := &mcpModernAdmission{
		target:   &store.MCPRouteTarget{AppID: appID},
		identity: auth.RuntimeIdentity{AppID: appID, Kind: store.AppKindExecution, HostedMCP: true},
		server:   FixtureServerMetadata{Name: "support", Title: "support", Version: "1.0.0", Description: "Support app"},
	}
	base := map[string]any{"name": "execute", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}}, "required": []string{"script"}}}
	catalogue := map[string]any{"tools": []any{base}}
	if err := appendMCPHostedCapabilityTools(context.Background(), catalogue, admission); err != nil {
		t.Fatal(err)
	}
	// One tool remains, with both raw script and authored app request variants.
	if len(catalogue["tools"].([]any)) != 1 || fixture.listed != 1 || base["inputSchema"].(map[string]any)["oneOf"] == nil {
		t.Fatalf("catalogue = %#v, schema reads = %d", catalogue, fixture.listed)
	}
	request := mcpJSONRPCRequest{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"name":"execute","arguments":{"operation":"execute","input":{"name":"Jane"}}}`)}
	response := httptest.NewRecorder()
	call := httptest.NewRequest("POST", "/mcp", nil)
	if !handleMCPHostedCapabilityCall(context.Background(), response, call, request, admission) {
		t.Fatal("authored execute was not handled")
	}
	assertMCPExecutionAppResult(t, response.Body.Bytes(), fixture)
}

// assertMCPExecutionAppResult checks that MCP projected the durable envelope and exact input.
func assertMCPExecutionAppResult(t *testing.T, response []byte, fixture *mcpExecutionAppFixture) {
	t.Helper()
	var envelope struct {
		Result struct {
			StructuredContent struct {
				ExecutionID string `json:"executionId"`
				Status      string `json:"status"`
				Output      struct {
					CustomerID string `json:"customerId"`
				} `json:"output"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		t.Fatal(err)
	}
	// The authored branch returns the same result identity and typed output as REST.
	if fixture.executed != 1 || string(fixture.input) != `{"name":"Jane"}` || envelope.Result.StructuredContent.ExecutionID == "" || envelope.Result.StructuredContent.Status != "succeeded" || envelope.Result.StructuredContent.Output.CustomerID != "cus_123" {
		t.Fatalf("response=%s calls=%d input=%s", response, fixture.executed, fixture.input)
	}
}

// TestMCPExecuteToolRejectsUnoptedSDK keeps ordinary SDK versions on the raw tool path.
func TestMCPExecuteToolRejectsUnoptedSDK(t *testing.T) {
	fixture := &mcpExecutionAppFixture{}
	SetMCPCapabilityAdapter(fixture)
	defer SetMCPCapabilityAdapter(nil)
	appID := uuid.New()
	admission := &mcpModernAdmission{target: &store.MCPRouteTarget{AppID: appID}, identity: auth.RuntimeIdentity{AppID: appID, Kind: store.AppKindSDK}}
	catalogue := map[string]any{"tools": []any{}}
	if err := appendMCPHostedCapabilityTools(context.Background(), catalogue, admission); err != nil {
		t.Fatal(err)
	}
	request := mcpJSONRPCRequest{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"name":"execute","arguments":{"operation":"execute","input":{"name":"Jane"}}}`)}
	response := httptest.NewRecorder()
	call := httptest.NewRequest("POST", "/mcp", nil)
	// The direct adapter cannot run authored code when the immutable token lacks HostedMCP.
	if !handleMCPHostedCapabilityCall(context.Background(), response, call, request, admission) || fixture.listed != 0 || fixture.executed != 0 {
		t.Fatalf("unopted adapter calls list=%d execute=%d", fixture.listed, fixture.executed)
	}
}
