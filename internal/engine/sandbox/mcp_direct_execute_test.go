package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMCPBundledRuntimeDirectExecution proves the advertised direct form reaches the normal authenticated bridge once.
func TestMCPBundledRuntimeDirectExecution(t *testing.T) {
	var calls atomic.Int32
	// The synthetic bridge checks routing and returns extra data to distinguish visible projection from saved state.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			OperationID string         `json:"operation_id"`
			Params      map[string]any `json:"params"`
		}
		// Direct mode must preserve the same session bearer and operation envelope as script-based call().
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.OperationID != "posts" || input.Params["status"] != "all" || r.URL.Path != "/mcp/call" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("direct execution bypassed the normal bridge contract")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":[{"title":"Post","content":"Full article"}]}`))
	}))
	defer bridge.Close()
	client := startMCPLimitRuntime(t, strings.TrimPrefix(bridge.URL, "http://127.0.0.1:"))
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "direct-execution-test", "version": "1"},
	})
	output := client.exchange(t, "tools/call", map[string]any{"name": "execute", "arguments": map[string]any{
		"operationId": "posts", "params": map[string]string{"status": "all"}, "select": []string{"title"}, "saveAs": "posts",
	}})
	// A single operation must not require a script, debug exception or second provider request for projection.
	if output.Result.IsError || len(output.Result.Content) != 1 || output.Result.Content[0].Text != `[{"title":"Post"}]` {
		t.Fatalf("direct output = %+v", output.Result)
	}
	read := client.execute(t, `return session.get("posts")[0].content;`)
	// Saved full data survives the visible field selection and is retrieved without replay.
	if read.Result.IsError || read.Result.Content[0].Text != `"Full article"` || calls.Load() != 1 {
		t.Fatalf("saved read = %+v, calls = %d", read.Result, calls.Load())
	}
	invalid := client.exchange(t, "tools/call", map[string]any{"name": "execute", "arguments": map[string]any{
		"operationId": "posts", "script": "return 1;",
	}})
	// Conflicting modes must reject before even the bridge sees another request.
	if !invalid.Result.IsError || !strings.Contains(invalid.Result.Content[0].Text, "MCP_EXECUTE_ARGUMENTS_INVALID") || calls.Load() != 1 {
		t.Fatalf("conflicting modes = %+v, calls = %d", invalid.Result, calls.Load())
	}
}

// TestMCPModernDirectExecution preserves authenticated admission and explicit state handles for direct calls.
func TestMCPModernDirectExecution(t *testing.T) {
	// The real adapter needs the shipped Node runtime to verify its argument and state envelopes.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for modern MCP coverage")
	}
	fixture := installMCPModernFixture(t)
	var calls atomic.Int32
	// Provider transport is synthetic; only authenticated adapter requests may reach it.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"title":"Post","content":"Full article"}}`))
	}))
	defer bridge.Close()
	previousPort := globalEnginePort
	globalEnginePort = strings.TrimPrefix(bridge.URL, "http://127.0.0.1:")
	t.Cleanup(func() { globalEnginePort = previousPort })
	arguments := map[string]any{"operationId": "posts", "select": []string{"title"}, "saveAs": "post"}
	denied := newMCPModernRequest(t, fixture, 80, "tools/call", map[string]any{"name": "execute", "arguments": arguments})
	denied.Header.Set("Authorization", "Bearer invalid")
	deniedResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(deniedResponse, denied)
	// New convenience arguments cannot bypass token admission or create provider side effects on rejection.
	if deniedResponse.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthorized direct request: status=%d calls=%d", deniedResponse.Code, calls.Load())
	}
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, newMCPModernRequest(t, fixture, 81, "tools/call", map[string]any{"name": "execute", "arguments": arguments}))
	result := decodeMCPModernResult(t, response)
	structured, _ := result["structuredContent"].(map[string]any)
	handle, _ := structured[mcpModernStateHandleArgument].(string)
	mcpModernToolHandles.RLock()
	sessionID := mcpModernToolHandles.byHandle[handle]
	mcpModernToolHandles.RUnlock()
	t.Cleanup(func() { terminateMCPSession(sessionID, "test_cleanup") })
	// A successful direct call returns the normal projected result and a token-bound continuation handle.
	if sessionID == "" || !strings.Contains(response.Body.String(), `\"title\":\"Post\"`) || strings.Contains(response.Body.String(), "Full article") || calls.Load() != 1 {
		t.Fatalf("modern direct result = %s, calls=%d", response.Body.String(), calls.Load())
	}
	read := httptest.NewRecorder()
	fixture.router.ServeHTTP(read, newMCPModernRequest(t, fixture, 82, "tools/call", map[string]any{"name": "execute", "arguments": map[string]any{
		"stateHandle": handle, "script": `return session.get("post").content;`,
	}}))
	// Explicit-handle continuation must read the full saved result without a repeated provider call.
	if !strings.Contains(read.Body.String(), "Full article") || calls.Load() != 1 {
		t.Fatalf("modern saved read = %s, calls=%d", read.Body.String(), calls.Load())
	}
}
