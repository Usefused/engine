package sandbox

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"net/http/httptest"
	"testing"
)

type mcpAttachedFixture struct {
	mcpUnifiedAppFixture
	alias string
}

// AttachedUnifiedApps advertises only the configured public alias and input contract.
func (f *mcpAttachedFixture) AttachedUnifiedApps(context.Context, auth.RuntimeIdentity) ([]models.UnifiedAppBinding, error) {
	return []models.UnifiedAppBinding{{Alias: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

// ExecuteAttachedUnifiedApp observes transport routing before the shared durable executor.
func (f *mcpAttachedFixture) ExecuteAttachedUnifiedApp(ctx context.Context, id auth.RuntimeIdentity, alias string, input json.RawMessage) (json.RawMessage, error) {
	f.alias = alias
	return f.ExecuteUnifiedApp(ctx, id, input)
}

// TestMCPAttachedUnifiedAppCall keeps one execute tool and routes the exact authored alias and input.
func TestMCPAttachedUnifiedAppCall(t *testing.T) {
	f := &mcpAttachedFixture{}
	SetMCPCapabilityAdapter(f)
	defer SetMCPCapabilityAdapter(nil)
	appID := uuid.New()
	admission := &mcpModernAdmission{target: &store.MCPRouteTarget{AppID: appID}, identity: auth.RuntimeIdentity{AppID: appID, Kind: store.AppKindMCP, HostedMCP: true}, server: FixtureServerMetadata{Name: "consumer", Version: "1.0.0"}}
	tool := map[string]any{"name": "execute", "inputSchema": map[string]any{"type": "object", "required": []string{"script"}}}
	catalogue := map[string]any{"tools": []any{tool}}
	if err := appendAttachedMCPTools(context.Background(), catalogue, admission); err != nil {
		t.Fatal(err)
	}
	schema, _ := json.Marshal(tool["inputSchema"])
	// The tool advertises typed input rather than requiring a caller to guess IDs or secrets.
	if !json.Valid(schema) || len(tool["inputSchema"].(map[string]any)["oneOf"].([]any)) != 2 {
		t.Fatalf("schema=%s", schema)
	}
	request := mcpJSONRPCRequest{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"name":"execute","arguments":{"operation":"unified_app:lookup","input":{"name":"Jane"}}}`)}
	response := httptest.NewRecorder()
	if !handleAttachedMCPCall(context.Background(), response, httptest.NewRequest("POST", "/mcp", nil), request, admission) {
		t.Fatal("attachment not handled")
	}
	assertMCPUnifiedAppResult(t, response.Body.Bytes(), &f.mcpUnifiedAppFixture)
	if f.alias != "lookup" {
		t.Fatalf("alias=%s", f.alias)
	}
	// Routing controls cannot be smuggled beside the typed input.
	request.Params = json.RawMessage(`{"name":"execute","arguments":{"operation":"unified_app:lookup","input":{},"selector":{}}}`)
	response = httptest.NewRecorder()
	handleAttachedMCPCall(context.Background(), response, httptest.NewRequest("POST", "/mcp", nil), request, admission)
	if response.Code != 400 || f.executed != 1 {
		t.Fatalf("invalid call status=%d executions=%d", response.Code, f.executed)
	}
}
