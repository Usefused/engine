package api

import (
	"testing"

	"github.com/google/uuid"
)

func TestMCPConfigApplyResponseUsesSharedExecutionTokenKey(t *testing.T) {
	planID := uuid.New()
	result := mcpConfigApplyResult{
		RuntimeID:      uuid.New(),
		RuntimeURL:     "https://engine.example/mcp/runtime/sse",
		ExecutionToken: "shown-once",
		ConfigKey:      "mcp:github:1.0.0",
		Name:           "github",
		Version:        "1.0.0",
	}

	response := mcpConfigApplyResponse(planID, result)
	if response["execution_token"] != result.ExecutionToken {
		t.Fatalf("execution_token = %v, want %q", response["execution_token"], result.ExecutionToken)
	}
	if _, exists := response["mcp_execution_token"]; exists {
		t.Fatal("MCP apply response must use the shared execution_token wire key")
	}
}

func TestMCPConfigApplyResponseOmitsTokenOnIdempotentApply(t *testing.T) {
	response := mcpConfigApplyResponse(uuid.New(), mcpConfigApplyResult{RuntimeID: uuid.New()})
	if _, exists := response["execution_token"]; exists {
		t.Fatal("idempotent MCP apply must not re-emit the execution token")
	}
}
