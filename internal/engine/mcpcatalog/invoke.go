package mcpcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"log/slog"
	"net/http"
	"time"
)

var ErrInvocation = errors.New("upstream MCP request failed; execution may have occurred; do not automatically retry")

// ValidateArguments validates the pinned JSON Schema without loading remote references.
func ValidateArguments(raw json.RawMessage, value any) error {
	var schema jsonschema.Schema
	// Only object tool contracts admitted by the provider are executable.
	if json.Unmarshal(raw, &schema) != nil || schema.Type != "object" {
		return errors.New("invalid imported MCP tool schema")
	}
	resolved, err := schema.Resolve(nil)
	// The default resolver performs no remote loads; unresolved schemas fail closed.
	if err != nil {
		return errors.New("unresolved imported MCP tool schema")
	}
	// Provider values must never appear in validation errors or telemetry.
	if resolved.Validate(value) != nil {
		return errors.New("arguments do not match the imported MCP tool schema")
	}
	return nil
}

// Invoke uses the discovery egress boundary and never forwards an app token or retries a provider call.
func Invoke(ctx context.Context, endpoint, token, method string, params json.RawMessage) (json.RawMessage, error) {
	// Endpoint validation precedes credential-bearing network construction.
	if err := ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}
	client, closeIdle := discoveryHTTPClient(endpoint, token)
	defer closeIdle()
	result, err := invokeWithClient(ctx, endpoint, client, method, params)
	// Reflected provider credentials cannot cross into an agent's results.
	encoded, _ := json.Marshal(token)
	if err != nil || token != "" && (bytes.Contains(result, []byte(token)) || bytes.Contains(result, encoded[1:len(encoded)-1])) {
		return nil, ErrInvocation
	}
	return result, nil
}

// invokeWithClient isolates protocol traffic for local fixture tests while production remains public-HTTPS-only.
func invokeWithClient(ctx context.Context, endpoint string, httpClient *http.Client, method string, params json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "Fused", Version: "1.0.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: 2 << 20}, nil)
	// Negotiation failures never expose provider diagnostics.
	if err != nil {
		return nil, ErrInvocation
	}
	defer session.Close()
	result, err := invokeSession(ctx, session, method, params)
	// Protocol failures carry a fixed message because remote errors can echo credentials.
	if err != nil {
		return nil, ErrInvocation
	}
	raw, err := json.Marshal(result)
	// The aggregate result is bounded in addition to each HTTP response.
	if err != nil || len(raw) > 2<<20 {
		return nil, ErrInvocation
	}
	return raw, nil
}

// invokeSession admits only the three selected capability operations, never an arbitrary JSON-RPC proxy.
func invokeSession(ctx context.Context, session *mcp.ClientSession, method string, raw json.RawMessage) (any, error) {
	// Each method retains its protocol argument type and content result shape.
	switch method {
	case "tools/call":
		var p mcp.CallToolParams
		// Malformed arguments must fail before provider execution.
		if json.Unmarshal(raw, &p) != nil {
			return nil, ErrInvocation
		}
		return session.CallTool(ctx, &p)
	case "prompts/get":
		var p mcp.GetPromptParams
		// Prompt argument values remain strings as required by MCP.
		if json.Unmarshal(raw, &p) != nil {
			return nil, ErrInvocation
		}
		return session.GetPrompt(ctx, &p)
	case "resources/read":
		var p mcp.ReadResourceParams
		// Resource URIs have already been checked against immutable membership.
		if json.Unmarshal(raw, &p) != nil {
			return nil, ErrInvocation
		}
		return session.ReadResource(ctx, &p)
	default:
		return nil, ErrInvocation
	}
}
