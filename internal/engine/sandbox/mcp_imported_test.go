package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestImportedCatalogTokenFiltering verifies search and native discovery expose the same exact token scope.
func TestImportedCatalogTokenFiltering(t *testing.T) {
	id := uuid.New()
	selection := models.SDKSelection{ServiceID: id, ImportedMCP: &models.ImportedMCPBinding{Tools: []json.RawMessage{json.RawMessage(`{"name":"echo","inputSchema":{"type":"object"}}`)}, Prompts: []json.RawMessage{json.RawMessage(`{"name":"echo"}`)}, Resources: []json.RawMessage{json.RawMessage(`{"name":"guide","uri":"fixture://guide"}`)}}}
	policy := store.AppTokenPolicy{AllowedOperations: []string{models.ImportedMCPOperation(id, "tool", "echo")}}
	operations, err := appendImportedFixtureOperations(nil, []models.SDKSelection{selection}, policy)
	// An exact tool grant is discoverable in search_docs under the same executable name.
	if err != nil || len(operations) != 1 || operations[0].OperationID != policy.AllowedOperations[0] {
		t.Fatalf("%+v %v", operations, err)
	}
	lists := importedNativeLists([]models.SDKSelection{selection}, auth.RuntimeIdentity{TokenPolicy: policy})
	// A same-named prompt and ungranted resource are independent capabilities.
	if len(lists["prompts"])+len(lists["resources"]) != 0 {
		t.Fatal("native discovery bypassed token scope")
	}
	policy.AllowAll = true
	caps := importedProtocolCapabilities([]models.SDKSelection{selection}, auth.RuntimeIdentity{TokenPolicy: policy})
	// Legacy initialization and modern discovery share the same capability projection.
	if len(caps) != 2 {
		t.Fatalf("capabilities=%v", caps)
	}
	raw := importedInitializeResponse(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2025-11-25"}}`, &Fixture{ImportedCapabilities: caps})
	if !strings.Contains(raw, `"prompts"`) || !strings.Contains(raw, `"tools"`) {
		t.Fatalf("initialize=%s", raw)
	}
}

// TestImportedContentNamespace preserves structured data and rewrites protocol resource references only.
func TestImportedContentNamespace(t *testing.T) {
	id := uuid.New()
	raw := NamespaceImportedMCPResult(id, json.RawMessage(`{"content":[{"type":"resource","resource":{"uri":"fixture://guide","text":"hello"}}],"structuredContent":{"uri":"unchanged"}}`))
	// User data is not a protocol link and must remain byte-equivalent in meaning.
	if !strings.Contains(string(raw), ImportedMCPResourcePrefix(id)+"fixture://guide") || !strings.Contains(string(raw), `"structuredContent":{"uri":"unchanged"}`) {
		t.Fatalf("result=%s", raw)
	}
}

// TestImportedExecutionReceiptPrivacy ensures provider diagnostics and concrete template values never enter activity metadata.
func TestImportedExecutionReceiptPrivacy(t *testing.T) {
	identity := auth.RuntimeIdentity{AccountID: uuid.New(), AppID: uuid.New(), AppFamilyID: uuid.New(), TokenID: uuid.New()}
	selection := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New()}
	event := importedMCPExecutionEvent(identity, selection, "resources/read", "private://customer/secret-value", time.Now(), errors.New("provider echoed secret-value"))
	raw, err := json.Marshal(event)
	// Canonical classification replaces raw diagnostics while retaining family and service attribution.
	if err != nil || strings.Contains(string(raw), "secret-value") || event.AppFamilyID != identity.AppFamilyID || event.ServiceID != selection.ServiceID || event.Status != models.EngineExecutionStatusFailed {
		t.Fatalf("unsafe or incorrectly attributed receipt: %s (%v)", raw, err)
	}
	// A protocol tool failure keeps its content result but records failed activity.
	if importedMCPResultError(json.RawMessage(`{"isError":true}`), nil) == nil || importedMCPResultError(json.RawMessage(`{"content":[]}`), nil) != nil {
		t.Fatal("MCP protocol outcome classification mismatch")
	}
}

// TestImportedNativeListsAndSSEAdmission covers empty-list wire shape and prevents bypassing the legacy handshake.
func TestImportedNativeListsAndSSEAdmission(t *testing.T) {
	lists := importedNativeLists(nil, auth.RuntimeIdentity{})
	result, handled, err := importedNativeRequest(context.Background(), nil, mcpJSONRPCRequest{Method: "prompts/list", Params: json.RawMessage(`{}`)}, auth.RuntimeIdentity{}, lists, nil)
	raw, _ := json.Marshal(result)
	// An empty MCP list must be an array, not null or an omitted property.
	if err != nil || !handled || string(raw) != `{"prompts":[]}` {
		t.Fatalf("empty native list: %s, handled=%v, err=%v", raw, handled, err)
	}
	response := httptest.NewRecorder()
	handled = handleImportedSSENative(context.Background(), response, &mcpSession{}, []byte(`{"jsonrpc":"2.0","id":1,"method":"prompts/list","params":{}}`))
	// Unregistered sessions are rejected before token validation or provider work.
	if !handled || response.Code != 400 || !strings.Contains(response.Body.String(), "initialized MCP session required") {
		t.Fatalf("SSE admission: handled=%v, status=%d, body=%s", handled, response.Code, response.Body.String())
	}
	fixture := &Fixture{ImportedCapabilities: map[string]any{"prompts": map[string]any{}}}
	original := `{"jsonrpc":"2.0","id":2,"result":{"content":[],"capabilities":"provider data"}}`
	// Tool results containing similarly named fields must remain untouched by initialization augmentation.
	if importedInitializeResponse(original, fixture) != original {
		t.Fatal("initialization projection modified a normal result")
	}
}
