package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestMCPTransportURLsForAppUsesForwardedPublicOrigin verifies stable and pinned URLs share the public origin.
func TestMCPTransportURLsForAppUsesForwardedPublicOrigin(t *testing.T) {
	familyID := uuid.New()
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "engine.example")

	urls := mcpTransportURLsForApp(request, familyID, appID, appID)
	wantStreamableHTTP := "https://engine.example/mcp/" + familyID.String()
	if urls.StreamableHTTP != wantStreamableHTTP {
		t.Fatalf("streamable HTTP URL = %q, want %q", urls.StreamableHTTP, wantStreamableHTTP)
	}
	if urls.SSE != wantStreamableHTTP+"/sse" {
		t.Fatalf("SSE URL = %q, want %q", urls.SSE, wantStreamableHTTP+"/sse")
	}
	wantPinned := "https://engine.example/mcp/" + appID.String()
	if urls.VersionedStreamableHTTP != wantPinned || urls.VersionedSSE != wantPinned+"/sse" {
		t.Fatalf("version-pinned transport URLs = %#v, want URLs rooted at %q", urls, wantPinned)
	}
}

// TestMCPTransportURLsForAppUsesRelativeURLsWithoutRequest keeps offline projections origin-neutral.
func TestMCPTransportURLsForAppUsesRelativeURLsWithoutRequest(t *testing.T) {
	familyID := uuid.New()
	appID := uuid.New()
	urls := mcpTransportURLsForApp(nil, familyID, appID, appID)
	wantStreamableHTTP := "/mcp/" + familyID.String()
	if urls.StreamableHTTP != wantStreamableHTTP || urls.SSE != wantStreamableHTTP+"/sse" {
		t.Fatalf("relative transport URLs = %#v", urls)
	}
	if urls.VersionedStreamableHTTP != "/mcp/"+appID.String() {
		t.Fatalf("relative pinned transport URL = %#v", urls)
	}
}

// TestMCPTransportURLsForAppOmitsUnavailableStableRoute prevents discovery from recommending a deactivated family target.
func TestMCPTransportURLsForAppOmitsUnavailableStableRoute(t *testing.T) {
	familyID, appID := uuid.New(), uuid.New()
	urls := mcpTransportURLsForApp(nil, familyID, appID, uuid.Nil)
	// Pinned access survives even though the family has no explicitly promoted version.
	if urls.StreamableHTTP != "" || urls.SSE != "" || urls.VersionedStreamableHTTP != "/mcp/"+appID.String() {
		t.Fatalf("transport URLs without stable target = %#v", urls)
	}
}

// TestMCPTransportURLsForAppUsesDirectRequestOrigin permits local HTTP while retaining stable identity.
func TestMCPTransportURLsForAppUsesDirectRequestOrigin(t *testing.T) {
	familyID := uuid.New()
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://127.0.0.1:8081/engine/graphql", nil)
	urls := mcpTransportURLsForApp(request, familyID, appID, appID)
	if urls.StreamableHTTP != "http://127.0.0.1:8081/mcp/"+familyID.String() {
		t.Fatalf("direct HTTP URL = %q", urls.StreamableHTTP)
	}

	request.TLS = &tls.ConnectionState{}
	urls = mcpTransportURLsForApp(request, familyID, appID, appID)
	if urls.StreamableHTTP != "https://127.0.0.1:8081/mcp/"+familyID.String() {
		t.Fatalf("direct HTTPS URL = %q", urls.StreamableHTTP)
	}
}

// TestMCPTransportURLsForAppUpgradesPublicHTTPOrigin prevents discovery from
// advertising a redirect that ordinary clients may follow without credentials.
func TestMCPTransportURLsForAppUpgradesPublicHTTPOrigin(t *testing.T) {
	familyID := uuid.New()
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "http")
	request.Header.Set("X-Forwarded-Host", "fused.run.usefused.com")

	urls := mcpTransportURLsForApp(request, familyID, appID, appID)
	want := "https://fused.run.usefused.com/mcp/" + familyID.String()
	if urls.StreamableHTTP != want || urls.SSE != want+"/sse" {
		t.Fatalf("public transport URLs = %#v, want TLS URLs rooted at %q", urls, want)
	}
}

// TestMCPTransportURLsForAppNormalizesForwardedHops uses only the first trusted proxy hop.
func TestMCPTransportURLsForAppNormalizesForwardedHops(t *testing.T) {
	familyID := uuid.New()
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "https, http")
	request.Header.Set("X-Forwarded-Host", "public.example:8443, proxy.internal")

	urls := mcpTransportURLsForApp(request, familyID, appID, appID)
	want := "https://public.example:8443/mcp/" + familyID.String()
	if urls.StreamableHTTP != want {
		t.Fatalf("forwarded multi-hop URL = %q, want %q", urls.StreamableHTTP, want)
	}
}

// TestMCPTransportURLsForAppRejectsInvalidForwardedOriginParts verifies an
// untrusted proxy hint cannot re-enable insecure public discovery.
func TestMCPTransportURLsForAppRejectsInvalidForwardedOriginParts(t *testing.T) {
	familyID := uuid.New()
	appID := uuid.New()
	request := httptest.NewRequest("GET", "http://engine.internal:8081/engine/graphql", nil)
	request.Header.Set("X-Forwarded-Proto", "javascript")
	request.Header.Set("X-Forwarded-Host", "attacker.example/path")

	urls := mcpTransportURLsForApp(request, familyID, appID, appID)
	want := "https://engine.internal:8081/mcp/" + familyID.String()
	if urls.StreamableHTTP != want {
		t.Fatalf("invalid forwarded origin fallback = %q, want %q", urls.StreamableHTTP, want)
	}
}
