package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
)

// MCPCapabilityAdapter connects the existing modern execute tool to the durable Execution App command.
type MCPCapabilityAdapter interface {
	ExecutionAppInputSchema(context.Context, auth.RuntimeIdentity) (json.RawMessage, bool, error)
	ExecuteExecutionApp(context.Context, auth.RuntimeIdentity, json.RawMessage) (json.RawMessage, error)
}

type mcpCapabilityAdapterHolder struct{ adapter MCPCapabilityAdapter }

var mcpCapabilityAdapter atomic.Pointer[mcpCapabilityAdapterHolder]

// ErrMCPCapabilityUnavailable distinguishes an absent immutable app execute contract from a runtime failure.
var ErrMCPCapabilityUnavailable = errors.New("MCP execution app is unavailable")

// SetMCPCapabilityAdapter installs the API-owned executor shared by SDK, REST, and MCP on this Engine process.
func SetMCPCapabilityAdapter(adapter MCPCapabilityAdapter) {
	// A nil adapter removes authored execution without affecting legacy MCP tools.
	if adapter == nil {
		mcpCapabilityAdapter.Store(nil)
		return
	}
	mcpCapabilityAdapter.Store(&mcpCapabilityAdapterHolder{adapter: adapter})
}

// admittedMCPCapabilityAdapter permits only exact authenticated Execution App versions opted into hosted MCP.
func admittedMCPCapabilityAdapter(admission *mcpModernAdmission) MCPCapabilityAdapter {
	// SDK and MCP-kind apps retain their existing transport behavior.
	if admission == nil || admission.target == nil || admission.identity.Kind != store.AppKindExecution || !admission.identity.HostedMCP || admission.identity.AppID != admission.target.AppID {
		return nil
	}
	holder := mcpCapabilityAdapter.Load()
	// Missing startup wiring cannot grant a second execution path.
	if holder == nil {
		return nil
	}
	return holder.adapter
}

// appendMCPHostedCapabilityTools adds the authored request variant to the existing execute tool.
func appendMCPHostedCapabilityTools(ctx context.Context, result map[string]any, admission *mcpModernAdmission) error {
	adapter := admittedMCPCapabilityAdapter(admission)
	// Legacy MCP and SDK apps keep their existing catalogue.
	if adapter == nil {
		return nil
	}
	inputSchema, found, err := adapter.ExecutionAppInputSchema(ctx, admission.identity)
	if err != nil {
		return err
	}
	// An Execution App version without an attached bundle still exposes selected raw MCP tools.
	if !found {
		return nil
	}
	// MCP tools/call arguments must remain an object even for a valid authored schema.
	if !validMCPToolInputSchema(inputSchema) {
		return errors.New("execution app input schema is invalid for MCP")
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return errors.New("MCP tool catalogue is invalid")
	}
	return extendMCPExecuteTool(tools, inputSchema)
}

// extendMCPExecuteTool preserves raw script mode and adds operation execute with typed input.
func extendMCPExecuteTool(tools []any, inputSchema json.RawMessage) error {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		// A malformed base descriptor cannot establish the union contract.
		if !ok {
			return errors.New("MCP tool catalogue is invalid")
		}
		if tool["name"] != "execute" {
			continue
		}
		base, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			return errors.New("MCP execute tool has no input schema")
		}
		tool["inputSchema"] = map[string]any{"type": "object", "oneOf": []any{base, map[string]any{
			"type": "object", "properties": map[string]any{
				"operation": map[string]any{"const": "execute"}, "input": inputSchema,
			}, "required": []string{"operation", "input"}, "additionalProperties": false,
		}}}
		tool["description"] = mcpModernExecuteToolDescription + " For this Execution App, pass operation=execute and typed input to run its authored function."
		// The two modes have different result envelopes, so a single output schema would mislead clients.
		delete(tool, "outputSchema")
		return nil
	}
	return errors.New("MCP execute tool is unavailable")
}

// validMCPToolInputSchema requires the object shape used by MCP tools/call arguments.
func validMCPToolInputSchema(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	// Scalar JSON schema cannot describe the authored input object.
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	return string(schema["type"]) == `"object"`
}

// handleMCPHostedCapabilityCall routes operation execute through the shared durable app command.
func handleMCPHostedCapabilityCall(ctx context.Context, w http.ResponseWriter, r *http.Request, request mcpJSONRPCRequest, admission *mcpModernAdmission) bool {
	input, handled, err := decodeMCPHostedExecute(request)
	// Script-based execute and other names retain the existing MCP runtime path.
	if !handled {
		return false
	}
	// Hosted execute retains the connected-user selector admission used by other modern tool calls.
	if _, selectorErr := mcpSessionAuthContext(r.Header); selectorErr != nil {
		writeMCPModernError(w, request.ID, -32602, selectorErr.Error(), http.StatusBadRequest, nil)
		return true
	}
	adapter := admittedMCPCapabilityAdapter(admission)
	// An unopted SDK cannot call authored code through the MCP transport.
	if adapter == nil {
		writeMCPModernError(w, request.ID, -32602, "unknown MCP tool", http.StatusBadRequest, nil)
		return true
	}
	if err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return true
	}
	envelope, err := adapter.ExecuteExecutionApp(ctx, admission.identity, input)
	// A missing exact-version bundle cannot become a child script fallback.
	if errors.Is(err, ErrMCPCapabilityUnavailable) {
		writeMCPModernError(w, request.ID, -32602, "execution app is unavailable", http.StatusBadRequest, nil)
		return true
	}
	if err != nil {
		writeMCPModernError(w, request.ID, -32603, "execution app runtime is unavailable", http.StatusServiceUnavailable, nil)
		return true
	}
	// The structured result carries the same execution ID, status, typed output, and read handle as REST.
	writeMCPModernResult(w, request.ID, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(envelope)}},
		"structuredContent": json.RawMessage(envelope),
	}, admission.server)
	return true
}

// decodeMCPHostedExecute selects only the app-level operation and rejects extra controls.
func decodeMCPHostedExecute(request mcpJSONRPCRequest) (json.RawMessage, bool, error) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	// Modern admission has already validated the routed name and envelope.
	if json.Unmarshal(request.Params, &params) != nil || params.Name != "execute" {
		return nil, false, nil
	}
	var route struct {
		Operation string `json:"operation"`
	}
	// Only explicit app-level operation execute diverts from the existing script tool.
	if json.Unmarshal(params.Arguments, &route) != nil || route.Operation != "execute" {
		return nil, false, nil
	}
	input, err := decodeMCPHostedExecuteArguments(params.Arguments)
	return input, true, err
}

// decodeMCPHostedExecuteArguments admits only typed input for the existing execute tool.
func decodeMCPHostedExecuteArguments(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var arguments struct {
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
	}
	// Additional state or script controls would make this execution ambiguous.
	if decoder.Decode(&arguments) != nil || arguments.Operation != "execute" || len(arguments.Input) == 0 || len(arguments.Input) > maxCapabilityInputBytes || !json.Valid(arguments.Input) {
		return nil, errors.New("execution app arguments are invalid")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("execution app arguments are invalid")
	}
	return arguments.Input, nil
}
