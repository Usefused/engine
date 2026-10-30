package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/shared/models"
	"net/http"
	"strings"
)

// MCPBridgeAdapter exposes immutable app bindings without admitting arbitrary local destinations.
type MCPBridgeAdapter interface {
	AttachedBridges(context.Context, auth.RuntimeIdentity) ([]models.BridgeBinding, error)
	ExecuteAttachedBridge(context.Context, auth.RuntimeIdentity, string, json.RawMessage) (json.RawMessage, error)
}

// bridgeMCPAdapter requires the same authenticated consumer identity as ordinary MCP execution.
func bridgeMCPAdapter(admission *mcpModernAdmission) MCPBridgeAdapter {
	// Authored Unified Apps retain their single public execute contract.
	if admission == nil || admission.target == nil || admission.identity.AppID != admission.target.AppID || admission.identity.Kind.String() == "unified_app" {
		return nil
	}
	holder := mcpCapabilityAdapter.Load()
	// Missing runtime wiring never exposes an unauthenticated fallback.
	if holder == nil {
		return nil
	}
	adapter, _ := holder.adapter.(MCPBridgeAdapter)
	return adapter
}

// appendBridgeMCPTools adds typed aliases to the existing execute tool rather than starting another runtime.
func appendBridgeMCPTools(ctx context.Context, result map[string]any, admission *mcpModernAdmission) error {
	adapter := bridgeMCPAdapter(admission)
	// Existing adapters without Bridge support keep their unchanged catalogue.
	if adapter == nil {
		return nil
	}
	bindings, err := adapter.AttachedBridges(ctx, admission.identity)
	// Catalogue failures must not silently hide selected capabilities.
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		return nil
	}
	tools, ok := result["tools"].([]any)
	// Only the established execute catalogue may be extended.
	if !ok {
		return errors.New("invalid MCP catalogue")
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		// Other MCP tools keep their existing schemas.
		if !ok || tool["name"] != "execute" {
			continue
		}
		variants := []any{tool["inputSchema"]}
		for _, b := range bindings {
			variants = append(variants, map[string]any{"type": "object", "properties": map[string]any{"operation": map[string]any{"const": "bridge:" + b.Alias}, "input": b.InputSchema}, "required": []string{"operation", "input"}, "additionalProperties": false})
		}
		tool["inputSchema"] = map[string]any{"type": "object", "oneOf": variants}
		tool["description"] = mcpModernExecuteToolDescription + " Call a selected local Bridge capability using bridge:alias and its typed input."
		delete(tool, "outputSchema")
		return nil
	}
	return errors.New("MCP execute tool unavailable")
}

// handleBridgeMCPCall dispatches a selected alias once and preserves uncertain command outcomes.
func handleBridgeMCPCall(ctx context.Context, w http.ResponseWriter, r *http.Request, request mcpJSONRPCRequest, admission *mcpModernAdmission) bool {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var args struct {
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
	}
	// Other operation namespaces keep their existing admission paths.
	if json.Unmarshal(request.Params, &params) != nil || params.Name != "execute" || json.Unmarshal(params.Arguments, &args) != nil || !strings.HasPrefix(args.Operation, "bridge:") {
		return false
	}
	adapter := bridgeMCPAdapter(admission)
	var fields map[string]json.RawMessage
	// Exact argument shape prevents injecting routing overrides alongside a legitimate alias.
	if adapter == nil || json.Unmarshal(params.Arguments, &fields) != nil || len(fields) != 2 || !validBridgeMCPInput(args.Input) {
		writeMCPModernError(w, request.ID, -32602, "invalid Bridge call", 400, nil)
		return true
	}
	result, err := adapter.ExecuteAttachedBridge(ContextWithMCPExecutionTransport(ctx), admission.identity, strings.TrimPrefix(args.Operation, "bridge:"), args.Input)
	// Failed local commands are never retried by the adapter.
	if err != nil {
		// Known profile refusals expose a fixed category without implying that the Bridge is offline.
		if bridgehost.IsProfileConstraintViolation(err) {
			writeMCPModernError(w, request.ID, -32602, bridgehost.ProfileConstraintMessage, 400, map[string]string{"code": bridgehost.ProfileConstraintCode})
			return true
		}
		writeMCPModernError(w, request.ID, -32602, "Bridge execution unavailable or outcome unknown; do not automatically retry commands", 400, nil)
		return true
	}
	writeMCPModernResult(w, request.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(result)}}, "structuredContent": map[string]any{"result": json.RawMessage(result)}}, admission.server)
	return true
}

// validBridgeMCPInput preserves scalar roots while enforcing the same bounded transport payload.
func validBridgeMCPInput(input json.RawMessage) bool { return len(input) > 0 && len(input) <= 1<<20 }
