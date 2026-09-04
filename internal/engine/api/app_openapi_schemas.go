package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemacontract"
	"github.com/Usefused/engine/internal/shared/schemaref"
)

type appOpenAPIExport struct {
	operations []appOpenAPIOperation
	components map[string]any
	err        error
}

type appOpenAPISchemaScope struct {
	export    *appOpenAPIExport
	namespace string
	index     *schemaref.Index
}

// physicalAppOpenAPIOperations shares one export namespace and admitted index
// per exact service version while preserving the selected operation ordering.
func physicalAppOpenAPIOperations(export *appOpenAPIExport, selections []models.SDKSelection, matches []store.ServiceContractEndpointMatch, metadata map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata) ([]appOpenAPIOperation, error) {
	operations := make([]appOpenAPIOperation, 0, len(matches))
	scopes := make(map[store.ServiceContractMetadataRef]*appOpenAPISchemaScope)
	for _, match := range matches {
		selection := selections[match.SelectionIndex]
		ref := store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
		scope := scopes[ref]
		// A shared dictionary is reused for every selected operation of its version.
		if scope == nil {
			scope = &appOpenAPISchemaScope{export: export, namespace: "Version_" + strings.ReplaceAll(ref.ServiceVersionID.String(), "-", "") + "_"}
			// Standalone historical schemas have no dictionary or negotiated references.
			if owner := metadata[ref]; owner != nil {
				scope.index = owner.DefinitionIndex
			}
			scopes[ref] = scope
		}
		operation, err := physicalAppOpenAPIOperation(match.Endpoint, scope)
		// No partial operation collection is published after a schema failure.
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, export.err
}

// loadAppOpenAPIMetadata fetches dictionaries only for selected operations that
// reference them, using the existing exact-version set-based snapshot read.
func loadAppOpenAPIMetadata(ctx context.Context, contracts appOpenAPIContractStore, selections []models.SDKSelection, matches []store.ServiceContractEndpointMatch) (map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	refs, err := appOpenAPISharedDefinitionRefs(selections, matches)
	// Standalone historical contracts do not need a dictionary lookup.
	if err != nil || len(refs) == 0 {
		return nil, err
	}
	batch, ok := contracts.(store.ServiceContractMetadataBatchStore)
	// A missing batch capability must not fall back to per-operation reads.
	if !ok {
		return nil, unavailableAppOpenAPISchemaError()
	}
	metadata, err := batch.ListServiceContractMetadata(ctx, refs)
	// Transport/store failures cannot be mistaken for a missing optional dictionary.
	if err != nil {
		return nil, unavailableAppOpenAPISchemaError()
	}
	for _, ref := range refs {
		// The schema owner is the immutable version, never the current service row.
		if err := validateAppOpenAPIMetadata(ref, metadata[ref]); err != nil {
			return nil, err
		}
	}
	return metadata, nil
}

// appOpenAPISharedDefinitionRefs deduplicates the already-selected operation
// owners before a single dictionary read; no stored rows are filtered in Go.
func appOpenAPISharedDefinitionRefs(selections []models.SDKSelection, matches []store.ServiceContractEndpointMatch) ([]store.ServiceContractMetadataRef, error) {
	seen := make(map[store.ServiceContractMetadataRef]bool)
	refs := make([]store.ServiceContractMetadataRef, 0)
	for _, match := range matches {
		shared := false
		err := schemacontract.Walk(match.Endpoint, func(schema *fusedobject.SchemaContract) error {
			shared = shared || schema.SharedDefinitions
			return nil
		})
		// Malformed typed containers cannot reach unbounded export processing.
		if err != nil {
			return nil, unavailableAppOpenAPISchemaError()
		}
		selection := selections[match.SelectionIndex]
		ref := store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
		// Many operations of one version share exactly one dictionary fetch.
		if shared && !seen[ref] {
			refs = append(refs, ref)
			seen[ref] = true
		}
	}
	return refs, nil
}

// validateAppOpenAPIMetadata prevents a store mismatch from silently binding a
// compact reference to a sibling version's definition with the same name.
func validateAppOpenAPIMetadata(ref store.ServiceContractMetadataRef, metadata *fusedobject.ServiceMetadata) error {
	// A requested dictionary must be present under its exact service/version owner.
	if metadata == nil || metadata.ID != ref.ServiceID || metadata.ServiceVersionID != ref.ServiceVersionID {
		return unavailableAppOpenAPISchemaError()
	}
	// Production metadata already has an admitted index; other stores use the same validator.
	if metadata.DefinitionIndex == nil {
		// Broken definition hashes remain a bounded public contract error.
		if err := schemacontract.PrepareDefinitions(metadata); err != nil {
			return unavailableAppOpenAPISchemaError()
		}
	}
	return nil
}

// unavailableAppOpenAPISchemaError keeps schema contents and internal store
// details out of the public control-plane error response.
func unavailableAppOpenAPISchemaError() error {
	return workspaceConfigHTTPError{
		status: http.StatusConflict, code: "app_openapi_schema_unavailable", category: "dependency",
		message:     "immutable operation schemas are unavailable or inconsistent",
		remediation: "Refresh the selected immutable service contracts, then retry the OpenAPI export.",
	}
}

// invalidAppOpenAPIProjectionError rejects immutable source shapes that Fused cannot truthfully map onto the shared REST execution envelope.
func invalidAppOpenAPIProjectionError(message string) error {
	return workspaceConfigHTTPError{
		status: http.StatusConflict, code: "app_openapi_projection_invalid", category: "validation",
		message:     message,
		remediation: "Select or import an immutable service version with a compatible operation schema, then create a new API version. Fused will not rewrite the source schema.",
	}
}

// schema registers a canonical root once and returns only its stable component
// reference, preserving cycles without recursively embedding definitions.
func (scope *appOpenAPISchemaScope) schema(contract *fusedobject.SchemaContract) map[string]any {
	// An absent schema remains unconstrained, unlike a broken declared reference.
	if contract == nil {
		return map[string]any{}
	}
	payload, err := appOpenAPISchemaPayload(contract)
	// A projection encoding failure must not become an unconstrained schema.
	if err != nil {
		scope.export.err = unavailableAppOpenAPISchemaError()
		return nil
	}
	key := scope.namespace + "Root_" + openAPISchemaIdentity(contract, payload)
	reference := map[string]any{"$ref": "#/components/schemas/" + key}
	// Repeated roots reuse the registered component before decoding their raw graph again.
	if _, exists := scope.export.components[key]; exists {
		return reference
	}
	root, err := decodeAppOpenAPISchemaRoot(payload)
	// Invalid raw schemas must not fall back to a less restrictive projection.
	if err != nil {
		scope.export.err = unavailableAppOpenAPISchemaError()
		return nil
	}
	index := scope.index
	// Local-only roots must not acquire access to a coincidentally named shared definition.
	if !contract.SharedDefinitions {
		index = nil
	}
	scope.addRoot(key, root, index)
	return reference
}

// appOpenAPISchemaPayload uses the reviewed projection only for historical
// contracts without raw bytes, never for an authored false or empty schema.
func appOpenAPISchemaPayload(contract *fusedobject.SchemaContract) ([]byte, error) {
	// An absent raw schema is different from an authored false or empty schema.
	if len(contract.Raw) == 0 {
		return json.Marshal(projectionOpenAPISchema(contract.Projection))
	}
	return contract.Raw, nil
}

// openAPISchemaIdentity includes dialect and reference scope because identical
// raw bytes must not inherit another schema's lookup or interpretation context.
func openAPISchemaIdentity(contract *fusedobject.SchemaContract, payload []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(contract.Dialect + "\x00" + strconv.FormatBool(contract.SharedDefinitions) + "\x00"))
	_, _ = digest.Write(payload)
	return hex.EncodeToString(digest.Sum(nil))
}

// decodeAppOpenAPISchemaRoot preserves exact raw numbers and boolean schemas
// without decoding a shared root again for every referencing operation.
func decodeAppOpenAPISchemaRoot(payload []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root any
	// Exact-number decoding avoids changing provider constraints during export.
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	// A valid schema cannot be followed by an ignored second document.
	if decoder.Decode(new(any)) != io.EOF {
		return nil, unavailableAppOpenAPISchemaError()
	}
	// JSON Schema admits only object or boolean roots; null/scalar data is not a schema.
	switch root.(type) {
	case map[string]any, bool:
		return root, nil
	default:
		return nil, unavailableAppOpenAPISchemaError()
	}
}

// collectOpenAPIBodyReference visits a root reference once to discover flat
// fields while retaining every nested property reference in serialized output.
func collectOpenAPIBodyReference(schema map[string]any, properties map[string]any, required *[]string, additionalProperties *any, scope *appOpenAPISchemaScope, seen map[string]bool, depth int) {
	ref, _ := schema["$ref"].(string)
	// Cyclic composition edges do not add another set of top-level body fields.
	if ref == "" || seen[ref] {
		return
	}
	seen[ref] = true
	collectOpenAPIBodyBranches(scope.resolve(ref), properties, required, additionalProperties, scope, seen, depth+1)
}

// addRoot reserves identity before relocating edges, so a recursive dictionary
// graph terminates after each unique definition has been visited once.
func (scope *appOpenAPISchemaScope) addRoot(key string, root any, index *schemaref.Index) {
	scope.addSchema(key, root, root, key, index)
}

// addSchema gives a lifted local subschema its own component while retaining
// the authored root used to interpret all references inside that subschema.
func (scope *appOpenAPISchemaScope) addSchema(key string, node, root any, rootKey string, index *schemaref.Index) {
	// Existing roots and earlier failures must not allocate another copy of a graph.
	if _, exists := scope.export.components[key]; exists || scope.export.err != nil {
		return
	}
	scope.export.components[key] = node
	relocated, err := schemaref.RewriteReferences(node, func(ref string) (string, error) {
		return scope.reference(rootKey, root, index, ref)
	})
	// Broken edges fail the whole export; no partial schema is returned.
	if err != nil {
		scope.export.err = unavailableAppOpenAPISchemaError()
		return
	}
	scope.export.components[key] = relocated
}

// reference relocates only a pointer, preserving its document-relative suffix
// and never replacing a recursive edge with an unconstrained object.
func (scope *appOpenAPISchemaScope) reference(key string, root any, index *schemaref.Index, ref string) (string, error) {
	node, document, name, found := index.Resolve(root, ref)
	// External or absent references cannot become silently lossy export fallbacks.
	if !found {
		return "", unavailableAppOpenAPISchemaError()
	}
	// Local definitions retain precedence over the service dictionary namespace.
	if name == "" {
		return scope.localReference(key, root, node, index, ref), nil
	}
	target := scope.namespace + "Definition_" + openAPISchemaDigest(name)
	scope.addRoot(target, document, index)
	_, suffix, nested := strings.Cut(strings.TrimPrefix(ref, "#/$defs/"), "/")
	// A subschema pointer remains relative to its exact owning definition component.
	if nested {
		return scope.reference(target, document, index, "#/"+suffix)
	}
	return "#/components/schemas/" + target, nil
}

// localReference lifts local targets into standard OpenAPI components because
// some tooling cannot follow JSON Schema $defs inside its typed Schema Object.
func (scope *appOpenAPISchemaScope) localReference(key string, root, node any, index *schemaref.Index, ref string) string {
	// A root cycle keeps the already-reserved component rather than creating an alias chain.
	if ref == "#" {
		return "#/components/schemas/" + key
	}
	target := key + "_Local_" + openAPISchemaDigest(ref)
	scope.addSchema(target, node, root, key, index)
	return "#/components/schemas/" + target
}

// resolve follows only an exported local pointer for flat body-field inspection,
// not for serialization or recursive graph reconstruction.
func (scope *appOpenAPISchemaScope) resolve(ref string) map[string]any {
	root := map[string]any{"components": map[string]any{"schemas": scope.export.components}}
	value, _ := schemaref.ResolveLocal(root, ref)
	object, _ := value.(map[string]any)
	return object
}

// openAPISchemaDigest provides collision-resistant component names without
// exposing source definition identifiers or normalizing their identity.
func openAPISchemaDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
