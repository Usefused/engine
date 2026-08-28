package api

import (
	"testing"

	"github.com/google/uuid"
)

// TestMCPConfigApplyResponseUsesSharedExecutionTokenKey verifies one-time token
// compatibility alongside stable and pinned transport discovery.
func TestMCPConfigApplyResponseUsesSharedExecutionTokenKey(t *testing.T) {
	planID := uuid.New()
	result := mcpConfigApplyResult{
		RuntimeID:      uuid.New(),
		ExecutionToken: "shown-once",
		ConfigKey:      "mcp:github:1.0.0",
		Name:           "github",
		Version:        "1.0.0",
	}

	transportURLs := mcpTransportURLs{
		StreamableHTTP:          "https://engine.example/mcp/family",
		SSE:                     "https://engine.example/mcp/family/sse",
		VersionedStreamableHTTP: "https://engine.example/mcp/runtime",
		VersionedSSE:            "https://engine.example/mcp/runtime/sse",
	}
	response := mcpConfigApplyResponse(planID, result, transportURLs)
	// MCP must retain the family token's established wire key.
	if response["execution_token"] != result.ExecutionToken {
		t.Fatalf("execution_token = %v, want %q", response["execution_token"], result.ExecutionToken)
	}
	// A kind-specific secret key would fork CLI decoding and storage behavior.
	if _, exists := response["mcp_execution_token"]; exists {
		t.Fatal("MCP apply response must use the shared execution_token wire key")
	}
	// Both stable and pinned transports must survive the response projection unchanged.
	if response["default_transport"] != mcpDefaultTransport || response["transport_urls"] != transportURLs {
		t.Fatalf("transport discovery = %#v, want default %q and URLs %#v", response, mcpDefaultTransport, transportURLs)
	}
	// The apply response must identify the exact version now serving the family URL.
	if response["stable"] != true || response["stable_version_id"] != result.RuntimeID.String() {
		t.Fatalf("stable target = %#v/%#v", response["stable"], response["stable_version_id"])
	}
	// The removed scalar URL cannot represent stable and pinned choices safely.
	if _, exists := response["mcp_url"]; exists {
		t.Fatal("MCP apply response must not retain the ambiguous mcp_url field")
	}
}

// TestMCPConfigApplyResponseOmitsTokenOnIdempotentApply keeps plaintext credentials one-time only.
func TestMCPConfigApplyResponseOmitsTokenOnIdempotentApply(t *testing.T) {
	response := mcpConfigApplyResponse(uuid.New(), mcpConfigApplyResult{RuntimeID: uuid.New()}, mcpTransportURLs{})
	// Idempotent apply cannot recover or re-emit plaintext from the token hash.
	if _, exists := response["execution_token"]; exists {
		t.Fatal("idempotent MCP apply must not re-emit the execution token")
	}
}
