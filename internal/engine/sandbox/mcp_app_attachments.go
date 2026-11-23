package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/shared/models"
)

// MCPAppAttachmentAdapter keeps transport metadata separate from Engine-owned delegation and execution.
type MCPAppAttachmentAdapter interface {
	AttachedUnifiedApps(context.Context, auth.RuntimeIdentity) ([]models.UnifiedAppBinding, error)
	ExecuteAttachedUnifiedApp(context.Context, auth.RuntimeIdentity, string, json.RawMessage) (json.RawMessage, error)
}

// attachedMCPAdapter requires an authenticated exact consumer before exposing the shared executor.
func attachedMCPAdapter(admission *mcpModernAdmission) MCPAppAttachmentAdapter {
	// Direct Unified Apps retain their existing single-execute contract.
	if admission == nil || admission.target == nil || admission.identity.AppID != admission.target.AppID || admission.identity.Kind.String() == "unified_app" {
		return nil
	}
	holder := mcpCapabilityAdapter.Load()
	if holder == nil {
		return nil
	}
	adapter, _ := holder.adapter.(MCPAppAttachmentAdapter)
	return adapter
}

// appendAttachedMCPTools extends the existing execute tool with explicit, schema-checked app aliases.
func appendAttachedMCPTools(ctx context.Context, result map[string]any, admission *mcpModernAdmission) error {
	adapter := attachedMCPAdapter(admission)
	// Older adapters with no hosted references continue exposing the physical catalogue.
	if adapter == nil {
		return nil
	}
	bindings, err := adapter.AttachedUnifiedApps(ctx, admission.identity)
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		return nil
	}
	extraVariants := make([]any, 0, len(bindings))
	for _, binding := range bindings {
		// Each alias binds its own authored input; no arbitrary target can be supplied by a model.
		if !validMCPToolInputSchema(binding.InputSchema) {
			return errors.New("invalid Unified App input schema")
		}
		extraVariants = append(extraVariants, mcpExecuteAliasVariant("unified_app:"+binding.Alias, binding.InputSchema))
	}
	return appendMCPExecuteAliasVariants(
		result,
		extraVariants,
		" Call an attached Unified App using its unified_app:alias operation and typed input.",
	)
}

// mcpExecuteAliasVariant constructs a closed JSON Schema variant binding one
// exact operation alias to its declared input contract.
func mcpExecuteAliasVariant(operation string, inputSchema any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"operation": map[string]any{"const": operation}, "input": inputSchema},
		"required":             []string{"operation", "input"},
		"additionalProperties": false,
	}
}

// appendMCPExecuteAliasVariants appends extra oneOf schema variants onto the
// established MCP execute tool without altering sibling tools in the catalogue.
func appendMCPExecuteAliasVariants(result map[string]any, extraVariants []any, descriptionSuffix string) error {
	tools, ok := result["tools"].([]any)
	// Only the established execute catalogue may be extended.
	if !ok {
		return errors.New("invalid MCP catalogue")
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		// Non-execute MCP tools keep their existing schemas unchanged.
		if !ok || tool["name"] != "execute" {
			continue
		}
		variants := make([]any, 0, 1+len(extraVariants))
		variants = append(variants, tool["inputSchema"])
		variants = append(variants, extraVariants...)
		tool["inputSchema"] = map[string]any{"type": "object", "oneOf": variants}
		tool["description"] = mcpModernExecuteToolDescription + descriptionSuffix
		delete(tool, "outputSchema")
		return nil
	}
	return errors.New("MCP execute tool unavailable")
}

// handleAttachedMCPCall routes only explicitly named app capabilities through the common durable executor.
func handleAttachedMCPCall(ctx context.Context, w http.ResponseWriter, r *http.Request, request mcpJSONRPCRequest, admission *mcpModernAdmission) bool {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var args struct {
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
	}
	// Non-attachment tools keep their established validation and execution path.
	if json.Unmarshal(request.Params, &params) != nil || params.Name != "execute" || json.Unmarshal(params.Arguments, &args) != nil || !strings.HasPrefix(args.Operation, "unified_app:") {
		return false
	}
	adapter := attachedMCPAdapter(admission)
	// Reject unknown argument fields using the existing bounded hosted-call decoder.
	var fields map[string]json.RawMessage
	if adapter == nil || json.Unmarshal(params.Arguments, &fields) != nil || len(fields) != 2 || len(args.Input) == 0 || len(args.Input) > maxCapabilityInputBytes {
		writeMCPModernError(w, request.ID, -32602, "invalid attached Unified App call", http.StatusBadRequest, nil)
		return true
	}
	if _, err := mcpSessionAuthContext(r.Header); err != nil {
		writeMCPModernError(w, request.ID, -32602, "invalid session context", http.StatusBadRequest, nil)
		return true
	}
	result, err := adapter.ExecuteAttachedUnifiedApp(ctx, admission.identity, strings.TrimPrefix(args.Operation, "unified_app:"), args.Input)
	// A denied dependency cannot fall through into a script or a different app.
	if err != nil {
		writeMCPModernError(w, request.ID, -32602, "attached Unified App is unavailable", http.StatusBadRequest, nil)
		return true
	}
	writeMCPModernResult(w, request.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(result)}}, "structuredContent": json.RawMessage(result)}, admission.server)
	return true
}
