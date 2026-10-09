package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"net/http"
	"strings"
)

// ImportedMCPAdapter keeps provider credentials in Engine while transports expose only selected contracts.
type ImportedMCPAdapter interface {
	ImportedMCPScope(context.Context, auth.RuntimeIdentity) ([]models.SDKSelection, error)
	InvokeImportedMCP(context.Context, auth.RuntimeIdentity, string, string, json.RawMessage) (json.RawMessage, error)
}

// importedMCPAdapter reuses the registered Engine executor rather than introducing another runtime.
func importedMCPAdapter() ImportedMCPAdapter {
	holder := mcpCapabilityAdapter.Load()
	// Older test adapters may have no imported capability support.
	if holder == nil {
		return nil
	}
	adapter, _ := holder.adapter.(ImportedMCPAdapter)
	return adapter
}

// ImportedMCPResourcePrefix separates upstream resource identities across selected services.
func ImportedMCPResourcePrefix(service uuid.UUID) string {
	return "fused-mcp://" + service.String() + "/"
}

// NamespaceImportedMCPResult rewrites only MCP content references, leaving tool structured data unchanged.
func NamespaceImportedMCPResult(service uuid.UUID, raw json.RawMessage) json.RawMessage {
	var result map[string]any
	// Non-object results fail unchanged; protocol validation already belongs to the SDK.
	if json.Unmarshal(raw, &result) != nil {
		return raw
	}
	namespaceImportedContents(result["contents"], service)
	namespaceImportedContents(result["content"], service)
	// Prompt messages embed protocol content one level below the message envelope.
	if messages, ok := result["messages"].([]any); ok {
		for _, message := range messages {
			if value, ok := message.(map[string]any); ok {
				namespaceImportedContents([]any{value["content"]}, service)
			}
		}
	}
	output, _ := json.Marshal(result)
	return output
}

// namespaceImportedContents preserves each content block while qualifying its resource reference.
func namespaceImportedContents(value any, service uuid.UUID) {
	items, _ := value.([]any)
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		// Content arrays can contain text, images, or resources; only resource references have URIs.
		if !ok {
			continue
		}
		if uri, ok := item["uri"].(string); ok {
			item["uri"] = ImportedMCPResourcePrefix(service) + uri
		}
		if resource, ok := item["resource"].(map[string]any); ok {
			if uri, ok := resource["uri"].(string); ok {
				resource["uri"] = ImportedMCPResourcePrefix(service) + uri
			}
		}
	}
}

// appendImportedFixtureOperations gives search_docs the exact selected tool input schemas and stable call names.
func appendImportedFixtureOperations(operations []FixtureOperation, selections []models.SDKSelection, policy store.AppTokenPolicy) ([]FixtureOperation, error) {
	identity := auth.RuntimeIdentity{TokenPolicy: policy}
	for _, selection := range selections {
		// Physical services have no additional documentation entries.
		if selection.ImportedMCP == nil {
			continue
		}
		for _, raw := range selection.ImportedMCP.Tools {
			var tool struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			}
			// Persisted malformed definitions cannot become permissive call contracts.
			if json.Unmarshal(raw, &tool) != nil || tool.Name == "" {
				return nil, errors.New("invalid imported MCP definition")
			}
			name := models.ImportedMCPOperation(selection.ServiceID, "tool", tool.Name)
			// Discovery and execution share the same exact token grant.
			if !identity.AllowsOperation(name) {
				continue
			}
			operations = append(operations, FixtureOperation{OperationID: name, Name: tool.Name, Description: tool.Description, ServiceID: selection.ServiceID.String(), ServiceVersionID: selection.ServiceVersionID.String(), Method: "MCP", Parameters: []models.Parameter{}, Responses: models.Responses{}, ImportedInputSchema: tool.InputSchema})
		}
	}
	return operations, nil
}

// dispatchImportedMCPCall reauthenticates family tokens immediately before invoking a selected upstream tool.
func dispatchImportedMCPCall(ctx context.Context, sess *mcpSession, name string, params map[string]any) (json.RawMessage, error) {
	adapter := importedMCPAdapter()
	// Missing Engine adapters cannot turn a namespaced operation into a physical call.
	if adapter == nil || globalTokenValidator == nil {
		return nil, ErrMCPCapabilityUnavailable
	}
	id, err := uuid.Parse(sess.appID)
	if err != nil {
		return nil, ErrMCPCapabilityUnavailable
	}
	identity, err := globalTokenValidator.Validate(ctx, id, sess.token)
	// Revoked or narrowed tokens are checked again even for an existing session fixture.
	if err != nil {
		return nil, ErrMCPCapabilityUnavailable
	}
	args, _ := json.Marshal(params)
	return adapter.InvokeImportedMCP(ctx, identity, "tools/call", name, args)
}

// importedNativeLists projects token-visible prompt and resource definitions without credentials or upstream URLs.
func importedNativeLists(selections []models.SDKSelection, identity auth.RuntimeIdentity) map[string][]any {
	lists := map[string][]any{"prompts": {}, "resources": {}, "resourceTemplates": {}}
	for _, selection := range selections {
		// The immutable selection is the only authority; current service discovery is irrelevant.
		if selection.ImportedMCP == nil {
			continue
		}
		binding := selection.ImportedMCP
		for kind, items := range map[string][]json.RawMessage{"prompt": binding.Prompts, "resource": binding.Resources, "template": binding.ResourceTemplates} {
			for _, raw := range items {
				key, item := importedNativeItem(selection.ServiceID, kind, raw)
				// Restricted tokens must not discover a capability they cannot use.
				if item != nil && identity.AllowsOperation(key) {
					target := map[string]string{"prompt": "prompts", "resource": "resources", "template": "resourceTemplates"}[kind]
					lists[target] = append(lists[target], item)
				}
			}
		}
	}
	return lists
}

// importedNativeItem rewrites protocol identities while retaining the provider's reviewable metadata.
func importedNativeItem(service uuid.UUID, kind string, raw json.RawMessage) (string, map[string]any) {
	var item map[string]any
	// Invalid metadata is never advertised as an anonymous capability.
	if json.Unmarshal(raw, &item) != nil {
		return "", nil
	}
	field := map[string]string{"prompt": "name", "resource": "uri", "template": "uriTemplate"}[kind]
	original, _ := item[field].(string)
	key := models.ImportedMCPOperation(service, kind, original)
	// Prompts route by names; resources retain URI syntax and template expressions.
	if kind == "prompt" {
		item[field] = key
	} else {
		item[field] = ImportedMCPResourcePrefix(service) + original
	}
	return key, item
}

// handleImportedNative handles native capabilities beside existing event resources on both MCP transports.
func handleImportedNative(ctx context.Context, w http.ResponseWriter, request mcpJSONRPCRequest, identity auth.RuntimeIdentity, server *FixtureServerMetadata, events map[string]mcpEventResource) bool {
	// Tool calls keep the established sandbox and search paths.
	if !strings.HasPrefix(request.Method, "prompts/") && request.Method != "resources/list" && request.Method != "resources/templates/list" && request.Method != "resources/read" {
		return false
	}
	adapter := importedMCPAdapter()
	if adapter == nil {
		return false
	}
	selections, err := adapter.ImportedMCPScope(ctx, identity)
	// Failed exact-scope reads cannot degrade to broader discovery.
	if err != nil {
		writeImportedNativeResponse(w, request, nil, err, server)
		return true
	}
	lists := importedNativeLists(selections, identity)
	result, handled, err := importedNativeRequest(ctx, adapter, request, identity, lists, events)
	if handled {
		writeImportedNativeResponse(w, request, result, err, server)
	}
	return handled
}

// importedNativeRequest admits bounded list cursors and exact call parameters before upstream dispatch.
func importedNativeRequest(ctx context.Context, adapter ImportedMCPAdapter, request mcpJSONRPCRequest, identity auth.RuntimeIdentity, lists map[string][]any, events map[string]mcpEventResource) (any, bool, error) {
	listKey := map[string]string{"prompts/list": "prompts", "resources/list": "resources", "resources/templates/list": "resourceTemplates"}[request.Method]
	// Lists are finite immutable snapshots, not live upstream discovery.
	if listKey != "" {
		if err := admitMCPModernEmptyCursor(request.Params); err != nil {
			return nil, true, err
		}
		items := lists[listKey]
		// Existing webhook resources remain available in the same native resource list.
		if listKey == "resources" {
			for _, event := range sortedMCPEventResources(events) {
				items = append(items, event)
			}
		}
		return map[string]any{listKey: items}, true, nil
	}
	return invokeImportedNative(ctx, adapter, request, identity)
}

// invokeImportedNative separates fixed event URIs from namespaced upstream resources.
func invokeImportedNative(ctx context.Context, adapter ImportedMCPAdapter, request mcpJSONRPCRequest, identity auth.RuntimeIdentity) (any, bool, error) {
	var params struct {
		Name      string          `json:"name"`
		URI       string          `json:"uri"`
		Arguments json.RawMessage `json:"arguments"`
	}
	// Invalid protocol input must not select an upstream connection.
	if json.Unmarshal(request.Params, &params) != nil {
		return nil, true, errors.New("invalid MCP parameters")
	}
	name := params.Name
	if request.Method == "resources/read" {
		name = params.URI
		if !strings.HasPrefix(name, "fused-mcp://") {
			return nil, false, nil
		}
	}
	// Only native get/read are eligible for provider execution.
	if request.Method != "prompts/get" && request.Method != "resources/read" {
		return nil, false, nil
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage(`{}`)
	}
	raw, err := adapter.InvokeImportedMCP(ctx, identity, request.Method, name, params.Arguments)
	return raw, true, err
}

// writeImportedNativeResponse uses the negotiated transport envelope without exposing internal failures.
func writeImportedNativeResponse(w http.ResponseWriter, request mcpJSONRPCRequest, result any, err error, server *FixtureServerMetadata) {
	// Modern responses retain their required version and server metadata headers.
	if server != nil {
		if err != nil {
			writeMCPModernError(w, request.ID, -32602, "MCP capability unavailable or invalid arguments", http.StatusBadRequest, nil)
			return
		}
		// Native invocation results are protocol objects, including raw SDK result envelopes.
		raw, _ := json.Marshal(result)
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			writeMCPModernError(w, request.ID, -32603, "invalid MCP result", http.StatusBadGateway, nil)
			return
		}
		writeMCPModernResult(w, request.ID, object, *server)
		return
	}
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, "MCP capability unavailable or invalid arguments", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
}

// handleImportedSessionNative preserves legacy session authentication for Go-owned native methods.
func handleImportedSessionNative(ctx context.Context, w http.ResponseWriter, sess *mcpSession, request mcpJSONRPCRequest) bool {
	// Non-native requests continue straight into the shared Node runtime.
	if !strings.HasPrefix(request.Method, "prompts/") && !strings.HasPrefix(request.Method, "resources/") {
		return false
	}
	id, err := uuid.Parse(sess.appID)
	if err != nil || globalTokenValidator == nil {
		return false
	}
	identity, err := globalTokenValidator.Validate(ctx, id, sess.token)
	// Native execution cannot continue under revoked token authority.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32600, "MCP authorization failed", http.StatusUnauthorized)
		return true
	}
	events, err := loadMCPEventResources(ctx, sess.appID, identity)
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32600, "MCP resources unavailable", http.StatusBadRequest)
		return true
	}
	return handleImportedNative(ctx, w, request, identity, nil, events)
}

// importedProtocolCapabilities advertises only native namespaces containing token-visible entries.
func importedProtocolCapabilities(selections []models.SDKSelection, identity auth.RuntimeIdentity) map[string]any {
	lists := importedNativeLists(selections, identity)
	capabilities := map[string]any{}
	// Empty namespaces are omitted so clients do not offer unusable native features.
	if len(lists["prompts"]) > 0 {
		capabilities["prompts"] = map[string]any{}
	}
	if len(lists["resources"])+len(lists["resourceTemplates"]) > 0 {
		capabilities["resources"] = map[string]any{}
	}
	return capabilities
}

// importedInitializeResponse adds Engine-owned capabilities without altering SDK protocol negotiation.
func importedInitializeResponse(response string, fixture *Fixture) string {
	// Physical-only sessions retain their SDK-produced envelope byte-for-byte.
	if fixture == nil || len(fixture.ImportedCapabilities) == 0 {
		return response
	}
	var envelope map[string]any
	if json.Unmarshal([]byte(response), &envelope) != nil {
		return response
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return response
	}
	// Only initialization carries protocolVersion; normal tool results must remain untouched.
	if !isImportedInitializeResult(result) {
		return response
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok {
		capabilities = map[string]any{}
		result["capabilities"] = capabilities
	}
	for key, value := range fixture.ImportedCapabilities {
		capabilities[key] = value
	}
	raw, _ := json.Marshal(envelope)
	return string(raw)
}
