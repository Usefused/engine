package api

import (
	"testing"

	"github.com/google/uuid"
)

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
		StreamableHTTP: "https://engine.example/mcp/runtime",
		SSE:            "https://engine.example/mcp/runtime/sse",
	}
	response := mcpConfigApplyResponse(planID, result, transportURLs)
	if response["execution_token"] != result.ExecutionToken {
		t.Fatalf("execution_token = %v, want %q", response["execution_token"], result.ExecutionToken)
	}
	if _, exists := response["mcp_execution_token"]; exists {
		t.Fatal("MCP apply response must use the shared execution_token wire key")
	}
	if response["default_transport"] != mcpDefaultTransport || response["transport_urls"] != transportURLs {
		t.Fatalf("transport discovery = %#v, want default %q and URLs %#v", response, mcpDefaultTransport, transportURLs)
	}
	if _, exists := response["mcp_url"]; exists {
		t.Fatal("MCP apply response must not retain the ambiguous mcp_url field")
	}
}

func TestMCPConfigApplyResponseOmitsTokenOnIdempotentApply(t *testing.T) {
	response := mcpConfigApplyResponse(uuid.New(), mcpConfigApplyResult{RuntimeID: uuid.New()}, mcpTransportURLs{})
	if _, exists := response["execution_token"]; exists {
		t.Fatal("idempotent MCP apply must not re-emit the execution token")
	}
}
