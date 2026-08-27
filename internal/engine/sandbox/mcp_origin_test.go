package sandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMCPRequestOriginAllowed covers the browser-origin grammar without requiring authentication state.
func TestMCPRequestOriginAllowed(t *testing.T) {
	tests := []struct {
		name, host string
		origins    []string
		want       bool
	}{
		{name: "non-browser client", host: "engine.example", want: true},
		{name: "same HTTPS origin", origins: []string{"https://engine.example"}, host: "engine.example", want: true},
		{name: "same HTTP loopback origin", origins: []string{"http://127.0.0.1:8081"}, host: "127.0.0.1:8081", want: true},
		{name: "case-insensitive authority", origins: []string{"https://ENGINE.EXAMPLE"}, host: "engine.example", want: true},
		{name: "foreign authority", origins: []string{"https://hostile.example"}, host: "engine.example"},
		{name: "empty header", origins: []string{""}, host: "engine.example"},
		{name: "duplicate headers", origins: []string{"https://engine.example", "https://engine.example"}, host: "engine.example"},
		{name: "opaque browser origin", origins: []string{"null"}, host: "engine.example"},
		{name: "path is not an origin", origins: []string{"https://engine.example/path"}, host: "engine.example"},
		{name: "credential-bearing URL", origins: []string{"https://user@engine.example"}, host: "engine.example"},
		{name: "non-network scheme", origins: []string{"file://engine.example"}, host: "engine.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://"+test.host+"/mcp/app", nil)
			for _, origin := range test.origins {
				// Add preserves duplicate field-lines so admission is tested before an intermediary can collapse them.
				request.Header.Add("Origin", origin)
			}
			if got := mcpRequestOriginAllowed(request); got != test.want {
				t.Fatalf("origins %q for host %q allowed = %v, want %v", test.origins, test.host, got, test.want)
			}
		})
	}
}

// TestMCPTransportsRejectCrossOriginBeforeAuthentication proves every public MCP transport shares one admission boundary.
func TestMCPTransportsRejectCrossOriginBeforeAuthentication(t *testing.T) {
	tests := []struct {
		name, method, target string
		handler              http.HandlerFunc
	}{
		{name: "Streamable HTTP", method: http.MethodPost, target: "/mcp/app", handler: mcpStreamableHandler},
		{name: "legacy SSE stream", method: http.MethodGet, target: "/mcp/app/sse", handler: mcpSseHandler},
		{name: "legacy SSE message", method: http.MethodPost, target: "/mcp/message", handler: mcpMessageHandler},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, nil)
			request.Host = "engine.example"
			request.Header.Set("Origin", "https://hostile.example")
			response := httptest.NewRecorder()

			test.handler(response, request)

			// A cross-origin request must stop before missing credentials or session state choose another response.
			if response.Code != http.StatusForbidden || response.Body.String() != `{"error":"`+mcpOriginForbiddenCode+`"}`+"\n" {
				t.Fatalf("cross-origin response = %d/%s", response.Code, response.Body.String())
			}
		})
	}
}
