package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestMCPProviderStatusThroughBundledRuntime checks public success/error semantics through the actual Node execute bundle.
func TestMCPProviderStatusThroughBundledRuntime(t *testing.T) {
	// Cover success, caller rejection, throttling and upstream failure through the same bridge.
	for _, status := range []int{200, 404, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// The bridge uses the production buffered adapter rather than fabricating an MCP tool result.
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				buffer := engine.NewBoundedBufferStream(1024)
				_ = engine.SendResponseContract(buffer, status, "json")
				_ = buffer.Send([]byte(`{"message":"private-provider-detail"}`))
				result, err := mcpPhysicalBufferedResult(buffer)
				// Provider failures must become the bridge's error channel before execute serializes the result.
				if err != nil {
					writeMCPCallResult(w, http.StatusBadGateway, mcpCallResponse{Error: err.Error()})
					return
				}
				writeMCPCallResult(w, http.StatusOK, mcpCallResponse{Result: result})
			}))
			defer bridge.Close()
			fixture := &Fixture{Server: mcpRuntimeDocumentationServer, Operations: []FixtureOperation{{OperationID: "getValue", ServiceID: "fixture", Name: "Get value", Method: "GET", Path: "/value", Responses: models.Responses{}}}}
			client := startMCPLimitRuntimeWithFixture(t, strings.TrimPrefix(bridge.URL, "http://127.0.0.1:"), fixture)
			client.exchange(t, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "fused-provider-test", "version": "1"}})
			response := client.execute(t, `return await call("getValue", {});`)
			// HTTP status, never the body's JSON shape, determines whether execute reports success.
			if response.Result.IsError != (status >= 400) {
				t.Fatalf("status %d: %#v", status, response)
			}
			raw, _ := json.Marshal(response)
			// Error bodies must not leak into model-facing exceptions.
			if status >= 400 && strings.Contains(string(raw), "private-provider-detail") {
				t.Fatal("provider error body leaked")
			}
		})
	}
}
