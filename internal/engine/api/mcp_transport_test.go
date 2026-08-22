package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestMCPTransportURLsForAppUsesForwardedPublicOrigin(t *testing.T) {
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "engine.example")

	urls := mcpTransportURLsForApp(request, appID)
	wantStreamableHTTP := "https://engine.example/mcp/" + appID.String()
	if urls.StreamableHTTP != wantStreamableHTTP {
		t.Fatalf("streamable HTTP URL = %q, want %q", urls.StreamableHTTP, wantStreamableHTTP)
	}
	if urls.SSE != wantStreamableHTTP+"/sse" {
		t.Fatalf("SSE URL = %q, want %q", urls.SSE, wantStreamableHTTP+"/sse")
	}
}

func TestMCPTransportURLsForAppUsesRelativeURLsWithoutRequest(t *testing.T) {
	appID := uuid.New()
	urls := mcpTransportURLsForApp(nil, appID)
	wantStreamableHTTP := "/mcp/" + appID.String()
	if urls.StreamableHTTP != wantStreamableHTTP || urls.SSE != wantStreamableHTTP+"/sse" {
		t.Fatalf("relative transport URLs = %#v", urls)
	}
}

func TestMCPTransportURLsForAppUsesDirectRequestOrigin(t *testing.T) {
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://127.0.0.1:8081/engine/graphql", nil)
	urls := mcpTransportURLsForApp(request, appID)
	if urls.StreamableHTTP != "http://127.0.0.1:8081/mcp/"+appID.String() {
		t.Fatalf("direct HTTP URL = %q", urls.StreamableHTTP)
	}

	request.TLS = &tls.ConnectionState{}
	urls = mcpTransportURLsForApp(request, appID)
	if urls.StreamableHTTP != "https://127.0.0.1:8081/mcp/"+appID.String() {
		t.Fatalf("direct HTTPS URL = %q", urls.StreamableHTTP)
	}
}

func TestMCPTransportURLsForAppNormalizesForwardedHops(t *testing.T) {
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "https, http")
	request.Header.Set("X-Forwarded-Host", "public.example:8443, proxy.internal")

	urls := mcpTransportURLsForApp(request, appID)
	want := "https://public.example:8443/mcp/" + appID.String()
	if urls.StreamableHTTP != want {
		t.Fatalf("forwarded multi-hop URL = %q, want %q", urls.StreamableHTTP, want)
	}
}

func TestMCPTransportURLsForAppRejectsInvalidForwardedOriginParts(t *testing.T) {
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal:8081/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "javascript")
	request.Header.Set("X-Forwarded-Host", "attacker.example/path")

	urls := mcpTransportURLsForApp(request, appID)
	want := "http://engine.internal:8081/mcp/" + appID.String()
	if urls.StreamableHTTP != want {
		t.Fatalf("invalid forwarded origin fallback = %q, want %q", urls.StreamableHTTP, want)
	}
}
