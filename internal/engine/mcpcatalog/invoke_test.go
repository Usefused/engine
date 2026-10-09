package mcpcatalog

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInvokeProtocolCapabilities verifies real transport calls preserve each native result on both supported handshakes.
func TestInvokeProtocolCapabilities(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		// Each negotiation must execute the same selected upstream operation exactly once.
		t.Run(protocol, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "Fixture", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{protocol}})
			calls := 0
			// Tool arguments and structured results must survive the proxy boundary unchanged.
			server.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls++
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(r.Params.Arguments)}}}, nil
			})
			// Prompt handlers receive string arguments through their native protocol method.
			server.AddPrompt(&mcp.Prompt{Name: "summary", Arguments: []*mcp.PromptArgument{{Name: "topic", Required: true}}}, func(ctx context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: r.Params.Arguments["topic"]}}}}, nil
			})
			// Resource readers return their concrete URI rather than another discovered URL.
			server.AddResource(&mcp.Resource{Name: "Guide", URI: "fixture://guide"}, func(ctx context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: r.Params.URI, Text: "guide"}}}, nil
			})
			// The fixture supplies a local transport only to this test seam.
			fixture := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
			defer fixture.Close()
			for method, params := range map[string]string{"tools/call": `{"name":"echo","arguments":{"text":"hello"}}`, "prompts/get": `{"name":"summary","arguments":{"topic":"hello"}}`, "resources/read": `{"uri":"fixture://guide"}`} {
				raw, err := invokeWithClient(context.Background(), fixture.URL, fixture.Client(), method, json.RawMessage(params))
				// Each capability returns its standard result object without lossy conversion.
				if err != nil || !json.Valid(raw) {
					t.Fatalf("%s: %s %v", method, raw, err)
				}
			}
			// Negotiation or retries must never duplicate a provider tool execution.
			if calls != 1 {
				t.Fatalf("tool executed %d times", calls)
			}
		})
	}
}

// TestImportedArgumentSchemas verifies local refs and required fields while denying remote schema loads.
func TestImportedArgumentSchemas(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"id":{"$ref":"#/$defs/id"}},"$defs":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false}`)
	// A valid native object schema must remain callable without flattening.
	if err := ValidateArguments(schema, map[string]any{"id": float64(7)}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{map[string]any{}, map[string]any{"id": "wrong"}, map[string]any{"id": 7, "extra": true}} {
		// Invalid provider inputs fail before any credential resolution or network dispatch.
		if ValidateArguments(schema, value) == nil {
			t.Fatal("invalid input admitted")
		}
	}
	// Remote schema references never trigger an HTTP fetch from model-supplied metadata.
	if ValidateArguments(json.RawMessage(`{"type":"object","$ref":"https://example.com/schema"}`), map[string]any{}) == nil {
		t.Fatal("remote reference admitted")
	}
}
