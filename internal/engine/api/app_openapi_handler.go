package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/google/uuid"
)

const (
	maxAppOpenAPIBytes       = 16 << 20
	maxOpenAPISchemaRefDepth = 32
)

// appOpenAPIContractStore is the narrow immutable-snapshot read used by the
// export handler without widening the general Engine Store interface.
type appOpenAPIContractStore interface {
	ListServiceContractEndpointsForSelections(context.Context, []store.ServiceContractEndpointSelection, []string) ([]store.ServiceContractEndpointMatch, error)
}

type appOpenAPIOperation struct {
	name           string
	requestSchema  map[string]any
	responseSchema map[string]any
}

// AppOpenAPIHandler exports one exact SDK app as an OpenAPI 3.1 document.
func AppOpenAPIHandler(engineStore store.Store, contracts appOpenAPIContractStore) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		actor, app, err := lifecycleActorAndApp(request.Context(), engineStore, request)
		if err != nil {
			writeSDKConfigError(response, err)
			return
		}
		family, err := loadSDKOpenAPIFamily(request.Context(), engineStore, actor.AccountID, app)
		if err != nil {
			writeSDKConfigError(response, err)
			return
		}
		if contracts == nil {
			writeSDKConfigError(response, workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "app schema export is unavailable"})
			return
		}
		document, err := buildAppOpenAPIDocument(request.Context(), contracts, app, family, request.URL.Query().Get("operation"))
		if err != nil {
			writeSDKConfigError(response, err)
			return
		}
		response.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1.0")
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = response.Write(document)
	}
}

// loadSDKOpenAPIFamily verifies that the exact app belongs to an SDK family in
// the authenticated workspace before any contract schema is projected.
func loadSDKOpenAPIFamily(ctx context.Context, engineStore store.Store, accountID uuid.UUID, app *store.App) (*store.AppFamily, error) {
	if app == nil || app.AppFamilyID == uuid.Nil {
		return nil, workspaceConfigHTTPError{status: http.StatusNotFound, message: "app not found"}
	}
	family, err := engineStore.GetAppFamily(ctx, app.AppFamilyID)
	if err != nil || family == nil {
		return nil, workspaceConfigHTTPError{status: http.StatusNotFound, message: "app family not found"}
	}
	if family.AppFamilyID != app.AppFamilyID || family.AccountID != accountID || family.AccountID != app.AccountID || family.Kind != store.AppKindSDK {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "OpenAPI export requires an SDK app"}
	}
	if !app.Status.Runnable() {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "OpenAPI export requires a runnable app version"}
	}
	return family, nil
}

// buildAppOpenAPIDocument composes the one stable REST route from immutable
// physical selections and Unified definitions, then enforces the response cap.
func buildAppOpenAPIDocument(ctx context.Context, contracts appOpenAPIContractStore, app *store.App, family *store.AppFamily, operationFilter string) ([]byte, error) {
	filter, err := validateAppOpenAPIOperationFilter(operationFilter)
	if err != nil {
		return nil, err
	}
	export, err := loadAppOpenAPIOperations(ctx, contracts, app, filter)
	if err != nil {
		return nil, err
	}
	if len(export.operations) == 0 {
		return nil, missingAppOpenAPIOperationError(filter)
	}
	document := composeAppOpenAPIDocument(app, family, export)
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to encode app schema"}
	}
	if len(encoded) > maxAppOpenAPIBytes {
		return nil, workspaceConfigHTTPError{status: http.StatusRequestEntityTooLarge, message: "app schema exceeds the export limit; use the operation filter"}
	}
	return encoded, nil
}

// loadAppOpenAPIOperations uses one endpoint query and, when required, one
// dictionary batch before combining integrity-checked Unified definitions.
func loadAppOpenAPIOperations(ctx context.Context, contracts appOpenAPIContractStore, app *store.App, operationFilter string) (*appOpenAPIExport, error) {
	if app == nil || app.ScopeSchemaVersion != models.AppScopeSchemaVersion {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app scope is incompatible"}
	}
	selections, err := decodeAppOpenAPISelections(app.Selections)
	if err != nil {
		return nil, err
	}
	endpointNames := optionalOperationNames(operationFilter)
	matches, err := contracts.ListServiceContractEndpointsForSelections(ctx, openAPIEndpointSelections(selections), endpointNames)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "failed to load immutable operation schemas"}
	}
	if err := validateAppOpenAPIMatches(selections, matches, operationFilter); err != nil {
		return nil, err
	}
	metadata, err := loadAppOpenAPIMetadata(ctx, contracts, selections, matches)
	// Shared references require one complete exact-version dictionary batch.
	if err != nil {
		return nil, err
	}
	export := &appOpenAPIExport{components: make(map[string]any)}
	export.operations, err = physicalAppOpenAPIOperations(export, selections, matches, metadata)
	// A physical schema failure prevents an apparently complete mixed export.
	if err != nil {
		return nil, err
	}
	unifiedScope := &appOpenAPISchemaScope{export: export, namespace: "Unified_" + strings.ReplaceAll(app.AppID.String(), "-", "") + "_"}
	unifiedOperations, err := unifiedAppOpenAPIOperations(app, operationFilter, unifiedScope)
	if err != nil {
		return nil, err
	}
	export.operations, err = rejectAmbiguousAppOpenAPIOperations(append(export.operations, unifiedOperations...))
	// Relocation errors remain fail-closed even when an otherwise valid operation exists.
	if err != nil {
		return nil, err
	}
	return export, export.err
}

// decodeAppOpenAPISelections rejects stale or malformed app selection state
// before it can widen the exported operation catalogue.
func decodeAppOpenAPISelections(payload []byte) ([]models.SDKSelection, error) {
	var selections []models.SDKSelection
	if len(payload) == 0 || json.Unmarshal(payload, &selections) != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app selections are unavailable"}
	}
	identities := make(map[store.ServiceContractMetadataRef]struct{}, len(selections))
	for _, selection := range selections {
		if selection.SchemaVersion != models.AppSelectionSchemaVersion || selection.ServiceID == uuid.Nil || selection.ServiceVersionID == uuid.Nil {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app selections are incompatible"}
		}
		identity := store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
		if _, duplicate := identities[identity]; duplicate || hasDuplicateAppOpenAPISelectionValues(selection) {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app selections are ambiguous"}
		}
		identities[identity] = struct{}{}
	}
	return selections, nil
}

// hasDuplicateAppOpenAPISelectionValues rejects duplicate immutable selectors
// before a malformed scope can duplicate rows in the set-based snapshot read.
func hasDuplicateAppOpenAPISelectionValues(selection models.SDKSelection) bool {
	endpointIDs := make(map[uuid.UUID]struct{}, len(selection.EndpointIDs))
	for _, endpointID := range selection.EndpointIDs {
		if endpointID == uuid.Nil {
			return true
		}
		if _, duplicate := endpointIDs[endpointID]; duplicate {
			return true
		}
		endpointIDs[endpointID] = struct{}{}
	}
	return hasDuplicateStrings(selection.OperationNames)
}

// hasDuplicateStrings reports empty or repeated exact operation selectors.
func hasDuplicateStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return true
		}
		if _, duplicate := seen[value]; duplicate {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

// openAPIEndpointSelections preserves exact selection order and identities for
// the set-based immutable endpoint lookup.
func openAPIEndpointSelections(selections []models.SDKSelection) []store.ServiceContractEndpointSelection {
	requests := make([]store.ServiceContractEndpointSelection, len(selections))
	for index, selection := range selections {
		requests[index] = store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
			SelectAll: selection.SelectAll, EndpointIDs: selection.EndpointIDs, OperationNames: selection.OperationNames,
		}
	}
	return requests
}

// validateAppOpenAPIMatches fails closed if a contract store returns rows
// outside the exact immutable selection or omits an explicitly named row.
func validateAppOpenAPIMatches(selections []models.SDKSelection, matches []store.ServiceContractEndpointMatch, filter string) error {
	foundNames := make([]map[string]struct{}, len(selections))
	foundIDs := make([]map[uuid.UUID]struct{}, len(selections))
	for index := range selections {
		foundNames[index], foundIDs[index] = make(map[string]struct{}), make(map[uuid.UUID]struct{})
	}
	for _, match := range matches {
		if !validAppOpenAPIMatch(selections, match, filter, foundNames, foundIDs) {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: "immutable operation schemas are inconsistent"}
		}
	}
	if missingExplicitAppOpenAPISelection(selections, foundNames, foundIDs, filter) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "immutable operation schemas are incomplete"}
	}
	return nil
}

// validAppOpenAPIMatch verifies selection index, exact filter, membership, and
// duplicate identity while recording rows used by completeness checks.
func validAppOpenAPIMatch(selections []models.SDKSelection, match store.ServiceContractEndpointMatch, filter string, foundNames []map[string]struct{}, foundIDs []map[uuid.UUID]struct{}) bool {
	if match.SelectionIndex < 0 || match.SelectionIndex >= len(selections) || match.Endpoint.ID == uuid.Nil || match.Endpoint.Name == "" {
		return false
	}
	if filter != "" && match.Endpoint.Name != filter {
		return false
	}
	selection := selections[match.SelectionIndex]
	if !appOpenAPISelectionContains(selection, match.Endpoint) {
		return false
	}
	if _, duplicate := foundIDs[match.SelectionIndex][match.Endpoint.ID]; duplicate {
		return false
	}
	foundIDs[match.SelectionIndex][match.Endpoint.ID] = struct{}{}
	foundNames[match.SelectionIndex][match.Endpoint.Name] = struct{}{}
	return true
}

// appOpenAPISelectionContains applies the same explicit ID/name/select-all
// admission semantics as the set-based store query.
func appOpenAPISelectionContains(selection models.SDKSelection, endpoint fusedobject.Endpoint) bool {
	if selection.SelectAll {
		return true
	}
	for _, endpointID := range selection.EndpointIDs {
		if endpointID == endpoint.ID {
			return true
		}
	}
	for _, operationName := range selection.OperationNames {
		if operationName == endpoint.Name {
			return true
		}
	}
	return false
}

// missingExplicitAppOpenAPISelection checks only selectors the bounded query
// can prove should be present; an ID for another filtered name is not missing.
func missingExplicitAppOpenAPISelection(selections []models.SDKSelection, foundNames []map[string]struct{}, foundIDs []map[uuid.UUID]struct{}, filter string) bool {
	for index, selection := range selections {
		for _, name := range selection.OperationNames {
			if (filter == "" || filter == name) && !openAPIStringSetContains(foundNames[index], name) {
				return true
			}
		}
		if filter == "" && missingEndpointID(selection.EndpointIDs, foundIDs[index]) {
			return true
		}
	}
	return false
}

// openAPIStringSetContains reports exact membership in a string set.
func openAPIStringSetContains(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}

// missingEndpointID reports whether an explicit immutable endpoint ID was not
// returned by the full snapshot query.
func missingEndpointID(expected []uuid.UUID, found map[uuid.UUID]struct{}) bool {
	for _, endpointID := range expected {
		if _, ok := found[endpointID]; !ok {
			return true
		}
	}
	return false
}

// unifiedAppOpenAPIOperations verifies the complete persisted definition set
// before projecting only the requested credential-free public operation.
func unifiedAppOpenAPIOperations(app *store.App, operationFilter string, scope *appOpenAPISchemaScope) ([]appOpenAPIOperation, error) {
	if len(app.UnifiedDefinitions) == 0 {
		return nil, nil
	}
	if app.UnifiedDefinitionSchemaVersion != unified.DefinitionSchemaVersion {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "Unified definitions are incompatible"}
	}
	hash, err := unifiedCanonicalHash(app.UnifiedDefinitions)
	if err != nil || hash != app.UnifiedDefinitionHash {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "Unified definitions failed integrity validation"}
	}
	definitions, err := unified.DecodeDefinitions(app.UnifiedDefinitions, unified.DefaultLimits())
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "Unified definitions are invalid"}
	}
	operations := make([]appOpenAPIOperation, 0, len(definitions))
	for _, definition := range definitions {
		if operationFilter != "" && definition.Name != operationFilter {
			continue
		}
		operations = append(operations, unifiedAppOpenAPIOperation(definition, scope))
	}
	return operations, nil
}

// rejectAmbiguousAppOpenAPIOperations keeps the discriminator honest when an
// app contains duplicate physical names or a physical/Unified collision.
func rejectAmbiguousAppOpenAPIOperations(operations []appOpenAPIOperation) ([]appOpenAPIOperation, error) {
	seen := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		if _, exists := seen[operation.name]; exists {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app contains an ambiguous operation name"}
		}
		seen[operation.name] = struct{}{}
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].name < operations[j].name })
	return operations, nil
}

// validateAppOpenAPIOperationFilter validates the exact optional name before
// it influences either the physical snapshot query or Unified projection.
func validateAppOpenAPIOperationFilter(filter string) (string, error) {
	if filter == "" {
		return "", nil
	}
	if filter != strings.TrimSpace(filter) || len(filter) > 512 {
		return "", workspaceConfigHTTPError{status: http.StatusBadRequest, message: "operation filter is invalid"}
	}
	return filter, nil
}

// optionalOperationNames converts the optional exact filter into the bounded
// store allowlist while nil continues to mean the full immutable selection.
func optionalOperationNames(filter string) []string {
	if filter == "" {
		return nil
	}
	return []string{filter}
}

// missingAppOpenAPIOperationError distinguishes an empty app from an absent
// exact filter without revealing any operation beyond the authorized app.
func missingAppOpenAPIOperationError(filter string) error {
	if filter != "" {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "operation not found"}
	}
	return workspaceConfigHTTPError{status: http.StatusUnprocessableEntity, message: "app has no executable operations"}
}

// physicalAppOpenAPIOperation builds the flat Engine input contract used by
// generated SDK calls and the direct REST execution route.
func physicalAppOpenAPIOperation(endpoint fusedobject.Endpoint, scope *appOpenAPISchemaScope) (appOpenAPIOperation, error) {
	if endpoint.Name == "" || endpoint.Name != strings.TrimSpace(endpoint.Name) || len(endpoint.Name) > maxRESTOperationBytes {
		return appOpenAPIOperation{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "physical operation name is unavailable"}
	}
	input, err := physicalOpenAPIInputSchema(endpoint, scope)
	if err != nil {
		return appOpenAPIOperation{}, err
	}
	return appOpenAPIOperation{
		name:           endpoint.Name,
		requestSchema:  executionRequestSchema(endpoint.Name, input, false, nil),
		responseSchema: physicalOpenAPIResponseSchema(endpoint, scope),
	}, nil
}

// unifiedAppOpenAPIOperation projects one private definition without exposing
// its mappings, dependency expressions, or provider routing identities.
func unifiedAppOpenAPIOperation(definition unified.OperationDefinition, scope *appOpenAPISchemaScope) appOpenAPIOperation {
	targets := make([]string, 0, len(definition.Bindings))
	services := make(map[string]struct{}, len(definition.Bindings))
	for _, binding := range definition.Bindings {
		targets = append(targets, binding.PublicTarget)
		service := binding.ServiceTarget
		if service == "" {
			service = binding.PublicTarget
		}
		services[service] = struct{}{}
	}
	input := scope.schema(&fusedobject.SchemaContract{Raw: definition.InputSchema})
	response := unifiedOpenAPIResponseSchema(definition.Name, targets)
	if definition.Output != nil {
		response = scope.schema(&fusedobject.SchemaContract{Raw: definition.Output.Schema})
	}
	return appOpenAPIOperation{
		name:           definition.Name,
		requestSchema:  executionRequestSchema(definition.Name, input, true, unifiedRoutingSchema(targets, services)),
		responseSchema: response,
	}
}

// executionRequestSchema creates one discriminator branch for the stable REST
// envelope while keeping physical and Unified routing fields distinct.
func executionRequestSchema(operation string, input map[string]any, isUnified bool, routing map[string]any) map[string]any {
	properties := map[string]any{
		"operation": map[string]any{"type": "string", "const": operation, "enum": []string{operation}},
		"input":     input,
	}
	required := []string{"operation", "input"}
	if isUnified {
		properties["targets"] = routing["targets"]
		properties["selectors"] = routing["selectors"]
		properties["target_pagination"] = routing["target_pagination"]
		required = append(required, "targets")
	} else {
		properties["selector"] = map[string]any{"$ref": "#/components/schemas/ExecutionSelector"}
		properties["pagination"] = map[string]any{"$ref": "#/components/schemas/PaginationIntent"}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

// unifiedRoutingSchema emits only declared public targets and service selector
// namespaces; private binding identities never enter the document.
func unifiedRoutingSchema(targets []string, services map[string]struct{}) map[string]any {
	sort.Strings(targets)
	selectorProperties := make(map[string]any, len(services))
	for service := range services {
		selectorProperties[service] = map[string]any{"$ref": "#/components/schemas/ExecutionSelector"}
	}
	paginationProperties := make(map[string]any, len(targets))
	for _, target := range targets {
		paginationProperties[target] = map[string]any{"$ref": "#/components/schemas/PaginationIntent"}
	}
	return map[string]any{
		"targets": map[string]any{
			"type": "array", "minItems": 1, "maxItems": 16, "uniqueItems": true,
			"items": map[string]any{"type": "string", "enum": targets},
		},
		"selectors":         map[string]any{"type": "object", "additionalProperties": false, "properties": selectorProperties},
		"target_pagination": map[string]any{"type": "object", "additionalProperties": false, "properties": paginationProperties},
	}
}

// physicalOpenAPIInputSchema flattens declared provider parameters and body
// fields exactly as Engine execution accepts them.
func physicalOpenAPIInputSchema(endpoint fusedobject.Endpoint, scope *appOpenAPISchemaScope) (map[string]any, error) {
	properties := make(map[string]any, len(endpoint.Parameters)+1)
	required := make([]string, 0, len(endpoint.Parameters))
	for _, parameter := range endpoint.Parameters {
		if parameter.Name == "_headers" {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "physical operation uses a reserved input name"}
		}
		properties[parameter.Name] = parameterOpenAPISchema(parameter, scope)
		if parameter.Required {
			required = append(required, parameter.Name)
		}
	}
	additionalProperties, err := addOpenAPIBodyFields(properties, &required, endpoint.RequestContent, scope)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "physical operation request schema is unavailable"}
	}
	properties["_headers"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	sort.Strings(required)
	return map[string]any{
		"type": "object", "additionalProperties": additionalProperties,
		"required": uniqueStrings(required), "properties": properties,
	}, nil
}

// parameterOpenAPISchema uses the selected version's canonical schema scope,
// retaining raw references and falling back only when a raw schema is absent.
func parameterOpenAPISchema(parameter fusedobject.Parameter, scope *appOpenAPISchemaScope) map[string]any {
	if parameter.Schema != nil {
		return scope.schema(parameter.Schema)
	}
	media := make([]string, 0, len(parameter.Content))
	for name := range parameter.Content {
		media = append(media, name)
	}
	sort.Strings(media)
	for _, name := range media {
		if parameter.Content[name].Schema != nil {
			return scope.schema(parameter.Content[name].Schema)
		}
	}
	if parameter.Type != "" {
		return map[string]any{"type": parameter.Type}
	}
	return map[string]any{}
}

// addOpenAPIBodyFields merges the selected reviewed request representation into
// the same flat input object used by the Engine dispatcher.
func addOpenAPIBodyFields(properties map[string]any, required *[]string, content *fusedobject.RequestContent, scope *appOpenAPISchemaScope) (any, error) {
	if content == nil {
		return false, nil
	}
	representation, err := selectOpenAPIRequestRepresentation(content)
	if err != nil {
		return false, err
	}
	if representation.ItemSchema != nil {
		return addOpenAPIItemBody(properties, required, content, representation.ItemSchema, scope)
	}
	return addOpenAPISchemaBody(properties, required, content, representation, scope)
}

// addOpenAPIItemBody exposes sequential request items under the exact runtime
// payload parameter instead of assuming the historical default name.
func addOpenAPIItemBody(properties map[string]any, required *[]string, content *fusedobject.RequestContent, itemSchema *fusedobject.SchemaContract, scope *appOpenAPISchemaScope) (any, error) {
	name := openAPIPayloadParameter(content)
	if name == "_headers" {
		return false, errors.New("reserved payload parameter")
	}
	properties[name] = map[string]any{"type": "array", "items": scope.schema(itemSchema)}
	if content.Required {
		*required = append(*required, name)
	}
	return false, nil
}

// addOpenAPISchemaBody exposes object declarations as flat input fields and
// uses one payload field only when the schema has no object declarations.
func addOpenAPISchemaBody(properties map[string]any, required *[]string, content *fusedobject.RequestContent, representation fusedobject.RequestRepresentation, scope *appOpenAPISchemaScope) (any, error) {
	schema := scope.schema(representation.Schema)
	bodyProperties, bodyRequired, additionalProperties := collectOpenAPIBodySchema(schema, representation, scope)
	if _, reserved := bodyProperties["_headers"]; reserved {
		return false, errors.New("reserved body field")
	}
	if len(bodyProperties) > 0 || !isFalseOpenAPIAdditionalProperties(additionalProperties) {
		mergeOpenAPIBodyProperties(properties, bodyProperties)
		*required = append(*required, bodyRequired...)
		return additionalProperties, nil
	}
	name := openAPIPayloadParameter(content)
	if name == "_headers" {
		return false, errors.New("reserved payload parameter")
	}
	properties[name] = schema
	if content.Required {
		*required = append(*required, name)
	}
	return false, nil
}

// openAPIPayloadParameter returns the same default flat payload key used by
// Engine request building when a non-object body has no explicit name.
func openAPIPayloadParameter(content *fusedobject.RequestContent) string {
	name := strings.TrimSpace(content.PayloadParameter)
	if name == "" {
		return "body"
	}
	return name
}

// collectOpenAPIBodySchema discovers flat body fields through the same bounded
// composition traversal as runtime admission and includes encoding-only keys.
func collectOpenAPIBodySchema(schema map[string]any, representation fusedobject.RequestRepresentation, scope *appOpenAPISchemaScope) (map[string]any, []string, any) {
	properties := make(map[string]any)
	required := make([]string, 0)
	additionalProperties := any(false)
	collectOpenAPIBodyBranches(schema, properties, &required, &additionalProperties, scope, make(map[string]bool), 0)
	for name := range representation.Encoding {
		if _, ok := properties[name]; !ok {
			properties[name] = map[string]any{}
		}
	}
	sort.Strings(required)
	return properties, uniqueStrings(required), additionalProperties
}

// collectOpenAPIBodyBranches recursively merges direct and composed object
// declarations without treating arbitrary nested object properties as flat.
func collectOpenAPIBodyBranches(schema map[string]any, properties map[string]any, required *[]string, additionalProperties *any, scope *appOpenAPISchemaScope, seen map[string]bool, depth int) {
	// Only root references/compositions are inspected; nested property graphs stay shared.
	if depth > maxOpenAPISchemaRefDepth {
		scope.export.err = unavailableAppOpenAPISchemaError()
		return
	}
	collectOpenAPIBodyReference(schema, properties, required, additionalProperties, scope, seen, depth)
	if direct, ok := schema["properties"].(map[string]any); ok {
		for name, property := range direct {
			mergeOpenAPIProperty(properties, name, property)
		}
	}
	*required = append(*required, stringSlice(schema["required"])...)
	mergeOpenAPIAdditionalProperties(additionalProperties, schema)
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		for _, branch := range openAPISchemaArray(schema[keyword]) {
			collectOpenAPIBodyBranches(branch, properties, required, additionalProperties, scope, seen, depth+1)
		}
	}
}

// mergeOpenAPIProperty preserves every composed declaration for a flat field
// rather than selecting a branch based on map iteration order.
func mergeOpenAPIProperty(properties map[string]any, name string, property any) {
	existing, found := properties[name]
	if !found {
		properties[name] = property
		return
	}
	properties[name] = map[string]any{"anyOf": []any{existing, property}}
}

// mergeOpenAPIAdditionalProperties retains a declared schema where possible;
// runtime admission treats any permissive composed branch as allowing fields.
func mergeOpenAPIAdditionalProperties(destination *any, schema map[string]any) {
	for _, keyword := range []string{"additionalProperties", "unevaluatedProperties"} {
		value, declared := schema[keyword]
		if !declared || value == false {
			continue
		}
		if value == true {
			*destination = true
			return
		}
		if allowed, ok := (*destination).(bool); ok && allowed {
			return
		}
		if existing, ok := (*destination).(map[string]any); ok {
			*destination = map[string]any{"anyOf": []any{existing, value}}
		} else {
			*destination = value
		}
	}
}

// mergeOpenAPIBodyProperties adds body declarations without overwriting an
// exact provider parameter that runtime dispatch will route first.
func mergeOpenAPIBodyProperties(destination, body map[string]any) {
	for name, property := range body {
		if _, parameter := destination[name]; !parameter {
			destination[name] = property
		}
	}
}

// isFalseOpenAPIAdditionalProperties recognizes the closed default used when
// no raw or projected body branch admits undeclared flat fields.
func isFalseOpenAPIAdditionalProperties(value any) bool {
	allowed, ok := value.(bool)
	return ok && !allowed
}

// openAPISchemaArray returns only object schema branches from a composition.
func openAPISchemaArray(value any) []map[string]any {
	items, _ := value.([]any)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if schema, ok := item.(map[string]any); ok {
			result = append(result, schema)
		}
	}
	return result
}

// selectOpenAPIRequestRepresentation mirrors Engine's sole/default selection
// rule without inferring from array order when multiple media are declared.
func selectOpenAPIRequestRepresentation(content *fusedobject.RequestContent) (fusedobject.RequestRepresentation, error) {
	if len(content.Representations) == 1 {
		return content.Representations[0], nil
	}
	for _, representation := range content.Representations {
		if content.DefaultMediaType != "" && strings.EqualFold(content.DefaultMediaType, representation.MediaType) {
			return representation, nil
		}
	}
	return fusedobject.RequestRepresentation{}, errors.New("request representation is ambiguous")
}

// projectionOpenAPISchema maps the bounded execution projection back onto
// standard JSON Schema field names without inventing unsupported semantics.
func projectionOpenAPISchema(schema fusedobject.Schema) map[string]any {
	result := make(map[string]any)
	if schema.Ref != "" {
		result["$ref"] = schema.Ref
	}
	if schema.Type != "" {
		result["type"] = schema.Type
	}
	if schema.Format != "" {
		result["format"] = schema.Format
	}
	if len(schema.Required) > 0 {
		result["required"] = append([]string(nil), schema.Required...)
	}
	if len(schema.Properties) > 0 {
		properties := make(map[string]any, len(schema.Properties))
		for name, property := range schema.Properties {
			properties[name] = projectionOpenAPISchema(property)
		}
		result["properties"] = properties
	}
	if schema.Items != nil {
		result["items"] = projectionOpenAPISchema(*schema.Items)
	}
	if schema.AdditionalProperties != nil {
		result["additionalProperties"] = projectionOpenAPISchema(*schema.AdditionalProperties)
	}
	return result
}

// physicalOpenAPIResponseSchema describes the one buffered JSON value returned
// by successful physical REST execution.
func physicalOpenAPIResponseSchema(endpoint fusedobject.Endpoint, scope *appOpenAPISchemaScope) map[string]any {
	resultSchemas := physicalSuccessSchemas(endpoint, scope)
	itemSchema := map[string]any{}
	if len(resultSchemas) == 1 {
		itemSchema = resultSchemas[0]
	} else if len(resultSchemas) > 1 {
		variants := make([]any, len(resultSchemas))
		for index := range resultSchemas {
			variants[index] = resultSchemas[index]
		}
		itemSchema = map[string]any{"oneOf": variants}
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"app_id", "operation", "kind", "status_code", "results"},
		"properties": map[string]any{
			"app_id":      map[string]any{"type": "string", "format": "uuid"},
			"operation":   map[string]any{"type": "string", "const": endpoint.Name, "enum": []string{endpoint.Name}},
			"kind":        map[string]any{"type": "string", "const": "physical", "enum": []string{"physical"}},
			"status_code": map[string]any{"type": "integer", "minimum": 200, "maximum": 299},
			"results":     map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": itemSchema},
		},
	}
}

// physicalSuccessSchemas returns deterministic schemas for every documented
// successful provider representation.
func physicalSuccessSchemas(endpoint fusedobject.Endpoint, scope *appOpenAPISchemaScope) []map[string]any {
	statuses := make([]string, 0, len(endpoint.Responses))
	for status := range endpoint.Responses {
		if strings.HasPrefix(status, "2") {
			statuses = append(statuses, status)
		}
	}
	sort.Strings(statuses)
	var schemas []map[string]any
	for _, status := range statuses {
		for _, representation := range endpoint.Responses[status].Representations {
			if representation.Schema != nil {
				schemas = append(schemas, scope.schema(representation.Schema))
			}
		}
	}
	return schemas
}

// unifiedOpenAPIResponseSchema documents ordered target results and explicit
// rollback results without exposing private dependency mappings.
func unifiedOpenAPIResponseSchema(operation string, targets []string) map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"app_id", "operation", "kind", "results", "rollbacks"},
		"properties": map[string]any{
			"app_id":    map[string]any{"type": "string", "format": "uuid"},
			"operation": map[string]any{"type": "string", "const": operation, "enum": []string{operation}},
			"kind":      map[string]any{"type": "string", "const": "unified", "enum": []string{"unified"}},
			"results":   map[string]any{"type": "array", "items": unifiedResultItemSchema(targets, true)},
			"rollbacks": map[string]any{"type": "array", "items": unifiedResultItemSchema(targets, false)},
		},
	}
}

// unifiedResultItemSchema keeps result and rollback status vocabularies exact
// while leaving provider data under the declared target result.
func unifiedResultItemSchema(targets []string, forward bool) map[string]any {
	statuses := []string{"success", "error"}
	if forward {
		statuses = append(statuses, "skipped")
	}
	properties := map[string]any{
		"target":      map[string]any{"type": "string", "enum": targets},
		"status":      map[string]any{"type": "string", "enum": statuses},
		"error_code":  map[string]any{"type": "string"},
		"auth_action": unifiedAuthActionOpenAPISchema(),
	}
	if forward {
		properties["data"] = map[string]any{}
	} else {
		properties["triggered_by"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"target", "status"}, "properties": properties}
}

// unifiedAuthActionOpenAPISchema documents only the non-secret repair routing
// fields emitted by REST result projection.
func unifiedAuthActionOpenAPISchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"action":        map[string]any{"type": "string", "enum": []string{"connect", "reconnect", "select_resource"}},
			"bucket_id":     map[string]any{"type": "string", "format": "uuid"},
			"service_id":    map[string]any{"type": "string", "format": "uuid"},
			"end_user_ref":  map[string]any{"type": "string"},
			"connection_id": map[string]any{"type": "string", "format": "uuid"},
			"reason":        map[string]any{"type": "string"},
		},
	}
}

// composeAppOpenAPIDocument assembles one path with discriminator branches for
// all exact app operations and stable shared selector/error components.
func composeAppOpenAPIDocument(app *store.App, family *store.AppFamily, export *appOpenAPIExport) map[string]any {
	components := map[string]any{
		"ExecutionSelector": executionSelectorOpenAPISchema(),
		"PaginationIntent":  paginationIntentOpenAPISchema(),
		"ExecutionError":    executionErrorOpenAPISchema(),
	}
	for key, schema := range export.components {
		components[key] = schema
	}
	requestRefs, responseRefs, mapping := operationOpenAPIComponents(components, export.operations)
	return map[string]any{
		"openapi": "3.1.0", "jsonSchemaDialect": "https://json-schema.org/draft/2020-12/schema",
		"info": map[string]any{
			"title": family.DisplayName + " execution API", "version": app.Version,
			"description": "Generated from one immutable Fused SDK app version. Execution tokens and provider credentials are never embedded.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"paths":   map[string]any{"/v1/apps/{app_id}/executions": executionOpenAPIPath(app.AppID, requestRefs, responseRefs, mapping)},
		"components": map[string]any{
			"securitySchemes": map[string]any{"SDKExecutionToken": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "Fused SDK execution token"}},
			"schemas":         components,
		},
		"x-fused-app-id": app.AppID.String(), "x-fused-app-family-id": app.AppFamilyID.String(),
		"x-fused-app-name": family.DisplayName, "x-fused-app-version": app.Version,
		"x-fused-operation-count": len(export.operations),
	}
}

// paginationIntentOpenAPISchema exposes only the request-level page cap and the shared hard ceiling.
func paginationIntentOpenAPISchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"max_pages"},
		"properties": map[string]any{
			"max_pages": map[string]any{"type": "integer", "minimum": 1, "maximum": paginationpolicy.CeilingMaxPages},
		},
	}
}

// operationOpenAPIComponents assigns collision-proof deterministic component
// names and discriminator mappings to operation request/response branches.
func operationOpenAPIComponents(components map[string]any, operations []appOpenAPIOperation) ([]any, []any, map[string]any) {
	requestRefs := make([]any, 0, len(operations))
	responseRefs := make([]any, 0, len(operations))
	mapping := make(map[string]any, len(operations))
	for _, operation := range operations {
		key := openAPIOperationComponentKey(operation.name)
		requestName, responseName := key+"Request", key+"Response"
		components[requestName], components[responseName] = operation.requestSchema, operation.responseSchema
		requestRef := "#/components/schemas/" + requestName
		requestRefs = append(requestRefs, map[string]any{"$ref": requestRef})
		responseRefs = append(responseRefs, map[string]any{"$ref": "#/components/schemas/" + responseName})
		mapping[operation.name] = requestRef
	}
	return requestRefs, responseRefs, mapping
}

// openAPIOperationComponentKey creates a readable component label with a hash
// suffix so punctuation normalization cannot collide.
func openAPIOperationComponentKey(operation string) string {
	var label strings.Builder
	for _, character := range operation {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			label.WriteRune(character)
		} else {
			label.WriteByte('_')
		}
		if label.Len() >= 48 {
			break
		}
	}
	if label.Len() == 0 {
		label.WriteString("Operation")
	}
	digest := sha256.Sum256([]byte(operation))
	return label.String() + "_" + hex.EncodeToString(digest[:6])
}

// executionOpenAPIPath describes the single real Engine route rather than
// manufacturing virtual per-operation paths that no-code tools could not call.
func executionOpenAPIPath(appID uuid.UUID, requests, responses []any, mapping map[string]any) map[string]any {
	return map[string]any{
		"post": map[string]any{
			"operationId": "executeFusedApp", "summary": "Execute one app operation",
			"security": []any{map[string]any{"SDKExecutionToken": []any{}}},
			"parameters": []any{
				map[string]any{"name": "app_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid", "enum": []string{appID.String()}}},
				map[string]any{"name": "Idempotency-Key", "in": "header", "required": false, "schema": map[string]any{"type": "string", "maxLength": 256}},
			},
			"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"oneOf": requests, "discriminator": map[string]any{"propertyName": "operation", "mapping": mapping},
			}}}},
			"responses": executionOpenAPIResponses(responses),
		},
	}
}

// executionOpenAPIResponses uses one stable error envelope across every
// documented Engine classification.
func executionOpenAPIResponses(successes []any) map[string]any {
	responses := map[string]any{
		"200": map[string]any{"description": "Execution completed", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"oneOf": successes}}}},
	}
	for _, status := range []string{"400", "401", "403", "404", "409", "429", "500", "502", "503", "504"} {
		responses[status] = map[string]any{"description": "Engine execution error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/ExecutionError"}}}}
	}
	return responses
}

// executionSelectorOpenAPISchema freezes the only routing fields accepted by
// REST execution and deliberately excludes credentials and arbitrary headers.
func executionSelectorOpenAPISchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"environment":  map[string]any{"type": "string", "maxLength": 256},
			"end_user_ref": map[string]any{"type": "string", "maxLength": 256},
			"auth_type":    map[string]any{"type": "string", "enum": []string{"api_key", "oauth", "oidc", "basic", "bearer", "mtls"}},
			"auth_name":    map[string]any{"type": "string", "maxLength": 256},
			"resource_id":  map[string]any{"type": "string", "format": "uuid"},
		},
	}
}

// executionErrorOpenAPISchema documents only the bounded public error envelope;
// provider response bodies and credentials never appear in this shape.
func executionErrorOpenAPISchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"error"},
		"properties": map[string]any{"error": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"code", "message"},
			"properties": map[string]any{
				"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
				"details": map[string]any{"type": "object", "additionalProperties": true},
			},
		}},
	}
}

// stringSlice converts decoded JSON string arrays without accepting mixed
// values that could corrupt required-field projection.
func stringSlice(value any) []string {
	if values, ok := value.([]string); ok {
		return append([]string(nil), values...)
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

// uniqueStrings preserves deterministic required-field output after provider
// parameters and body declarations are merged.
func uniqueStrings(values []string) []string {
	unique := values[:0]
	for index, value := range values {
		if index == 0 || values[index-1] != value {
			unique = append(unique, value)
		}
	}
	return unique
}
