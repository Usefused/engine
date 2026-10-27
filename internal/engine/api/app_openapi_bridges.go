package api

import (
	"context"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"strings"
)

// buildStoredAppOpenAPIDocument loads local schemas from the exact successful app publication.
func buildStoredAppOpenAPIDocument(ctx context.Context, engineStore store.Store, contracts appOpenAPIContractStore, bundles store.UnifiedAppBundleStore, app *store.App, family *store.AppFamily, filter string) ([]byte, error) {
	bindings, err := storedOpenAPIBridgeBindings(ctx, engineStore, app)
	// Missing immutable contracts cannot silently disappear from a mixed export.
	if err != nil {
		return nil, err
	}
	return buildAppOpenAPIDocumentWithBundle(ctx, contracts, bundles, app, family, filter, bindings...)
}

// storedOpenAPIBridgeBindings reads immutable local contracts even when a lightweight app projection omits capability keys.
func storedOpenAPIBridgeBindings(ctx context.Context, engineStore store.Store, app *store.App) ([]models.BridgeBinding, error) {
	count := 0
	for _, key := range app.CapabilityKeys {
		// Capability keys are server-generated and keep local scope separate from provider names.
		if strings.HasPrefix(key, "bridge:") {
			count++
		}
	}
	repository, ok := engineStore.(store.BridgeBindingStore)
	// Provider-only legacy adapters have no local contract storage, while known Bridge scope must fail closed.
	if !ok {
		if count > 0 {
			return nil, unavailableAppOpenAPISchemaError()
		}
		return nil, nil
	}
	bindings, err := repository.ReadBridgeBindings(ctx, app.AccountID, app.AppID)
	// Every immutable local capability must have its preserved contract.
	if err != nil || (count > 0 && len(bindings) != count) {
		return nil, unavailableAppOpenAPISchemaError()
	}
	return bindings, nil
}

// appendBridgeOpenAPIOperations exposes unary aliases and preserves their pinned JSON Schema references.
func appendBridgeOpenAPIOperations(export *appOpenAPIExport, bindings []models.BridgeBinding, filter string) error {
	for _, binding := range bindings {
		name := "bridge:" + binding.Alias
		// Events use the SDK receiver and cannot be advertised as callable REST operations.
		if binding.Kind == "event" || filter != "" && filter != name {
			continue
		}
		// Corrupt aliases cannot create misleading public operation identifiers.
		if !unifiedAppAlias.MatchString(binding.Alias) {
			return invalidAppOpenAPIProjectionError("Bridge alias is unavailable")
		}
		scope := &appOpenAPISchemaScope{export: export, namespace: "Bridge_" + openAPIOperationComponentKey(name) + "_"}
		input := scope.schema(&fusedobject.SchemaContract{Raw: binding.InputSchema})
		output := scope.schema(&fusedobject.SchemaContract{Raw: binding.OutputSchema})
		// Shared export rewriting keeps local refs valid after schemas move into OpenAPI components.
		if export.err != nil {
			return export.err
		}
		request := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"operation", "input"}, "properties": map[string]any{
			"operation": map[string]any{"type": "string", "const": name, "enum": []string{name}}, "input": input,
		}}
		export.operations = append(export.operations, appOpenAPIOperation{name: name, requestSchema: request, responseSchema: bridgeOpenAPIResponseSchema(name, output)})
	}
	return nil
}

// bridgeOpenAPIResponseSchema matches the shared REST envelope while retaining the exact local result schema.
func bridgeOpenAPIResponseSchema(operation string, output map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"app_id", "operation", "kind", "status_code", "results"}, "properties": map[string]any{
		"app_id":      map[string]any{"type": "string", "format": "uuid"},
		"operation":   map[string]any{"type": "string", "const": operation, "enum": []string{operation}},
		"kind":        map[string]any{"type": "string", "const": "bridge", "enum": []string{"bridge"}},
		"status_code": map[string]any{"type": "integer", "const": 200},
		"results":     map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": output},
	}}
}
