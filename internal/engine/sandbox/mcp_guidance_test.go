package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
)

// TestMCPGuidanceModernAdmission proves optional guidance stays behind normal app authorization without starting Node.
func TestMCPGuidanceModernAdmission(t *testing.T) {
	fixture := installMCPModernFixture(t)
	t.Setenv("PATH", t.TempDir())
	active := activeMCPSessionCount()
	for i, item := range []struct {
		method string
		params map[string]any
	}{
		{"prompts/list", nil},
		{"prompts/get", map[string]any{"name": mcpGuidance.Guide.Name}},
		{"resources/list", nil},
		{"resources/read", map[string]any{"uri": mcpGuidance.Guide.URI}},
	} {
		response := httptest.NewRecorder()
		fixture.router.ServeHTTP(response, newMCPModernRequest(t, fixture, i+100, item.method, item.params))
		result := decodeMCPModernResult(t, response)
		// Shared guidance cannot make token-scoped catalogues publicly cacheable.
		if strings.HasSuffix(item.method, "/list") && result["cacheScope"] != "private" {
			t.Fatalf("missing private catalogue cache scope: %s", item.method)
		}
		raw, err := json.Marshal(result)
		// Listing and retrieval must expose the shared guide rather than an independent transport copy.
		if err != nil || !strings.Contains(string(raw), "Fused") {
			t.Fatalf("guide result %s: %s (%v)", item.method, raw, err)
		}
		denied := newMCPModernRequest(t, fixture, i+200, item.method, item.params)
		denied.Header.Set("Authorization", "Bearer denied")
		response = httptest.NewRecorder()
		fixture.router.ServeHTTP(response, denied)
		// Even static guidance cannot bypass app lifecycle and token admission.
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized %s returned %d", item.method, response.Code)
		}
	}
	// Adding read-only guidance must never create execution state.
	if activeMCPSessionCount() != active {
		t.Fatal("guidance created an execution session")
	}
}

// TestMCPGuidanceNativeMerge keeps provider-authored entries intact beside the server-owned prompt and resource.
func TestMCPGuidanceNativeMerge(t *testing.T) {
	lists := map[string][]any{"prompts": {map[string]any{"name": "provider:example"}}, "resources": {map[string]any{"uri": "fused-mcp://provider/docs"}}}
	for _, method := range []string{"prompts/list", "resources/list"} {
		result, handled, err := importedNativeRequest(context.Background(), nil, mcpJSONRPCRequest{Method: method, Params: json.RawMessage(`{}`)}, auth.RuntimeIdentity{}, lists, nil)
		raw, _ := json.Marshal(result)
		// A guide must augment the selected provider namespace, never replace or rewrite it.
		if err != nil || !handled || !strings.Contains(string(raw), "provider") || !strings.Contains(string(raw), mcpGuidance.Guide.Name) {
			t.Fatalf("merged list %s: %s (%v)", method, raw, err)
		}
	}
	for _, method := range []string{"prompts/get", "resources/read"} {
		params, _ := json.Marshal(map[string]any{"name": mcpGuidance.Guide.Name, "uri": mcpGuidance.Guide.URI})
		result, handled, err := importedNativeRequest(context.Background(), nil, mcpJSONRPCRequest{Method: method, Params: params}, auth.RuntimeIdentity{}, lists, nil)
		raw, _ := json.Marshal(result)
		quoted, _ := json.Marshal(mcpGuidance.Guide.Text)
		// Both native transports deliver the exact UTF-8 guide bytes shared with the Node resource.
		if err != nil || !handled || !strings.Contains(string(raw), string(quoted)) {
			t.Fatalf("guide read %s: %s (%v)", method, raw, err)
		}
	}
}
