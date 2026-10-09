package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/mcpcatalog"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/yosida95/uritemplate/v3"
	"go.opentelemetry.io/otel"
	"strings"
)

// ImportedMCPScope rechecks exact app identity before exposing its immutable imported selections.
func (s *EngineGRPCServer) ImportedMCPScope(ctx context.Context, identity auth.RuntimeIdentity) ([]models.SDKSelection, error) {
	runtime, err := s.store.GetAppRuntime(ctx, identity.AppID)
	// Neither cross-account nor cross-family reads may borrow an authenticated app's identity.
	if err != nil || runtime == nil || runtime.AccountID != identity.AccountID || runtime.AppFamilyID != identity.AppFamilyID || runtime.Kind != identity.Kind {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	return models.DecodeAppSelections(runtime.ScopeSchemaVersion, runtime.Selections)
}

// InvokeImportedMCP dispatches only exact app-selected, token-authorized capabilities with Engine-owned credentials.
func (s *EngineGRPCServer) InvokeImportedMCP(ctx context.Context, identity auth.RuntimeIdentity, method, name string, arguments json.RawMessage) (json.RawMessage, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.mcp.imported_invoke")
	defer span.End()
	selections, err := s.ImportedMCPScope(ctx, identity)
	// Runtime reads cannot fall back to the mutable service catalog.
	if err != nil {
		return nil, err
	}
	selection, upstream, err := findImportedMCP(selections, identity, method, name, arguments)
	// Unknown names and token denials are indistinguishable before any credential read.
	if err != nil {
		return nil, err
	}
	token, err := s.importedMCPToken(ctx, identity, selection)
	// Credential failures must not become anonymous provider calls.
	if err != nil {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	// Provider work enters the shared execution gate and activity publisher after selection and credential admission.
	result, err := sandbox.RunImportedMCPInvocation(ctx, identity, selection, method, name, func(callCtx context.Context) (json.RawMessage, error) {
		// The closure contains only Engine-owned connection state, never an agent-supplied destination.
		return mcpcatalog.Invoke(callCtx, selection.ImportedMCP.URL, token, method, upstream)
	})
	// Embedded resource references retain the service namespace on their return trip.
	if err != nil {
		return nil, err
	}
	return sandbox.NamespaceImportedMCPResult(selection.ServiceID, result), nil
}

// importedMCPToken resolves the selected service's family bucket at execution time, never the importer's token.
func (s *EngineGRPCServer) importedMCPToken(ctx context.Context, identity auth.RuntimeIdentity, selection models.SDKSelection) (string, error) {
	name := selection.ImportedMCP.SecretName
	// Public MCP connections deliberately omit Authorization.
	if name == "" {
		return "", nil
	}
	bucket, err := s.store.ResolveAppFamilyServiceBucket(ctx, identity.AppFamilyID, selection.ServiceID)
	// Missing family bindings cannot borrow another service or actor's bucket.
	if err != nil || bucket == nil {
		return "", sandbox.ErrMCPCapabilityUnavailable
	}
	key, err := bucketSecretStorageKey(name)
	// The pinned reference must still satisfy the shared bucket secret grammar.
	if err != nil {
		return "", err
	}
	secret, err := s.store.GetSecret(ctx, bucket.BucketID, uuid.Nil, key)
	// Deleted secrets fail closed before decrypting or contacting the provider.
	if err != nil || secret == nil {
		return "", sandbox.ErrMCPCapabilityUnavailable
	}
	return decryptMCPCatalogToken(s.masterKey, secret)
}

// findImportedMCP resolves external names using the same namespace used for discovery and token grants.
func findImportedMCP(selections []models.SDKSelection, identity auth.RuntimeIdentity, method, name string, args json.RawMessage) (models.SDKSelection, json.RawMessage, error) {
	for _, selection := range selections {
		// Physical services contribute no imported authority.
		if selection.ImportedMCP == nil {
			continue
		}
		raw, matched, err := matchImportedMCP(selection, identity, method, name, args)
		// A matched but invalid request must not fall through to another service.
		if matched {
			return selection, raw, err
		}
	}
	return models.SDKSelection{}, nil, sandbox.ErrMCPCapabilityUnavailable
}

// matchImportedMCP checks exact identity first and schema membership before mapping to upstream params.
func matchImportedMCP(selection models.SDKSelection, identity auth.RuntimeIdentity, method, name string, args json.RawMessage) (json.RawMessage, bool, error) {
	binding := selection.ImportedMCP
	// Protocol kinds stay separate even when upstream names coincide.
	switch method {
	case "tools/call":
		return matchImportedNamed(selection.ServiceID, identity, "tool", binding.Tools, name, args)
	case "prompts/get":
		return matchImportedNamed(selection.ServiceID, identity, "prompt", binding.Prompts, name, args)
	case "resources/read":
		return matchImportedResource(selection.ServiceID, identity, binding, name)
	default:
		return nil, false, nil
	}
}

// matchImportedNamed preserves tool schemas and prompt arguments while replacing only the routed name.
func matchImportedNamed(service uuid.UUID, identity auth.RuntimeIdentity, kind string, items []json.RawMessage, name string, args json.RawMessage) (json.RawMessage, bool, error) {
	for _, raw := range items {
		var item struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		_ = json.Unmarshal(raw, &item)
		key := models.ImportedMCPOperation(service, kind, item.Name)
		// The exact fully-qualified grant is required before validating user input.
		if key != name {
			continue
		}
		if !identity.AllowsOperation(key) {
			return nil, true, sandbox.ErrMCPCapabilityUnavailable
		}
		var arguments map[string]any
		// Object arguments preserve MCP's tool and prompt contracts.
		if json.Unmarshal(args, &arguments) != nil || arguments == nil {
			return nil, true, errors.New("MCP arguments must be an object")
		}
		// Imported tools retain their original JSON Schema, including composition and local references.
		if kind == "tool" {
			if err := mcpcatalog.ValidateArguments(item.InputSchema, arguments); err != nil {
				return nil, true, err
			}
		}
		// Prompt descriptors constrain required names and string values even after upstream drift.
		if kind == "prompt" {
			if err := validateImportedPromptArguments(raw, arguments); err != nil {
				return nil, true, err
			}
		}
		result, _ := json.Marshal(map[string]any{"name": item.Name, "arguments": arguments})
		return result, true, nil
	}
	return nil, false, nil
}

// matchImportedResource authorizes a concrete URI against a selected fixed resource or URI template.
func matchImportedResource(service uuid.UUID, identity auth.RuntimeIdentity, binding *models.ImportedMCPBinding, name string) (json.RawMessage, bool, error) {
	prefix := sandbox.ImportedMCPResourcePrefix(service)
	// A foreign namespace cannot borrow this service's provider credentials.
	if !strings.HasPrefix(name, prefix) {
		return nil, false, nil
	}
	uri := strings.TrimPrefix(name, prefix)
	for kind, items := range map[string][]json.RawMessage{"resource": binding.Resources, "template": binding.ResourceTemplates} {
		for _, raw := range items {
			var item struct {
				URI      string `json:"uri"`
				Template string `json:"uriTemplate"`
			}
			_ = json.Unmarshal(raw, &item)
			key := item.URI
			matched := uri == key
			// Template matches are bounded by the reviewed template, never arbitrary URI forwarding.
			if kind == "template" {
				key = item.Template
				template, err := uritemplate.New(key)
				matched = err == nil && template.Match(uri) != nil
			}
			if matched && identity.AllowsOperation(models.ImportedMCPOperation(service, kind, key)) {
				result, _ := json.Marshal(map[string]string{"uri": uri})
				return result, true, nil
			}
		}
	}
	return nil, true, sandbox.ErrMCPCapabilityUnavailable
}

// validateImportedPromptArguments keeps prompt invocation inside its reviewed argument contract.
func validateImportedPromptArguments(raw json.RawMessage, arguments map[string]any) error {
	var prompt struct {
		Arguments []struct {
			Name     string `json:"name"`
			Required bool   `json:"required"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(raw, &prompt)
	allowed := map[string]bool{}
	for _, argument := range prompt.Arguments {
		allowed[argument.Name] = true
		// Required arguments cannot be silently omitted after a descriptor refresh.
		if _, exists := arguments[argument.Name]; argument.Required && !exists {
			return errors.New("missing required MCP prompt argument")
		}
	}
	for name, value := range arguments {
		_, isString := value.(string)
		// MCP prompt values are strings; undeclared arguments are never forwarded.
		if !allowed[name] || !isString {
			return errors.New("invalid MCP prompt argument")
		}
	}
	return nil
}
