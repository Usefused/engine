package api

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	maxAppScaffoldOperationsPerSelection = 512
	maxAppScaffoldOperationsTotal        = 4096
	maxAppScaffoldServiceKeyBytes        = 256
	maxAppScaffoldVersionBytes           = 128
)

var errAppScaffoldRequirementsUnavailable = errors.New("app scaffold requirements unavailable")

type appScaffoldSelection struct {
	Service    string
	Version    string
	Operations []string
	SelectAll  bool
}

type appScaffoldRequirement struct {
	Service  string
	Variable string
}

type appScaffoldRequirementStore interface {
	store.AppScaffoldSelectionStore
	store.ServiceContractMetadataBatchStore
	store.ServiceContractEndpointSelectionBatchStore
	store.WorkspaceExecutionPolicyBatchStore
}

var appScaffoldSelectionGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "AppScaffoldSelectionInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"service":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"version":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"operations": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
		"select_all": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var appScaffoldRequirementGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppScaffoldRequirement",
	Fields: graphql.Fields{
		"service":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"variable": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

// appScaffoldRequirementsGraphQLField exposes only unresolved variable names
// so SDK and MCP generators can share one credential-free initialization read.
func appScaffoldRequirementsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appScaffoldRequirementGraphQLType))),
		Args: graphql.FieldConfigArgument{
			"selections": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appScaffoldSelectionGraphQLInput)))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app_scaffold_requirements")
			defer span.End()
			selections, err := decodeAppScaffoldSelections(p.Args["selections"])
			// Invalid input is recorded only as a failed count-bearing operation;
			// authoring keys and operation names never enter telemetry.
			if err != nil {
				span.SetStatus(codes.Error, "app scaffold requirements unavailable")
				return nil, err
			}
			span.SetAttributes(
				attribute.Int("engine.app_scaffold.selection_count", len(selections)),
				attribute.Int("engine.app_scaffold.operation_count", appScaffoldOperationCount(selections)),
			)
			repository, ok := s.(appScaffoldRequirementStore)
			// A partial repository would invite scalar fallbacks and N+1 reads, so
			// rollout fails closed until every bounded batch surface is available.
			if !ok {
				span.SetStatus(codes.Error, "app scaffold requirements unavailable")
				return nil, errAppScaffoldRequirementsUnavailable
			}
			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
			// Authorization failures are returned without attempting any store read.
			if err != nil {
				span.SetStatus(codes.Error, "app scaffold requirements unavailable")
				return nil, err
			}
			requirements, err := loadAppScaffoldRequirements(ctx, repository, authorized, selections)
			// Store and contract failures share one public error so unavailable and
			// unauthorized service references cannot become workspace membership probes.
			if err != nil {
				span.SetStatus(codes.Error, "app scaffold requirements unavailable")
				return nil, errAppScaffoldRequirementsUnavailable
			}
			span.SetAttributes(
				attribute.Int("engine.app_scaffold.resolved_service_count", len(selections)),
				attribute.Int("engine.app_scaffold.unresolved_variable_count", len(requirements)),
			)
			return projectAppScaffoldRequirements(requirements), nil
		},
	}
}

// decodeAppScaffoldSelections validates and canonicalizes the bounded GraphQL
// batch before any SQL or snapshot work can be scheduled.
func decodeAppScaffoldSelections(raw any) ([]appScaffoldSelection, error) {
	items, ok := raw.([]interface{})
	// The schema normally guarantees this shape; the check also protects direct
	// resolver tests and future adapters from bypassing GraphQL coercion.
	if !ok || len(items) == 0 || len(items) > store.MaxAppScaffoldSelections {
		return nil, errAppScaffoldRequirementsUnavailable
	}
	selections := make([]appScaffoldSelection, 0, len(items))
	seenServices := make(map[string]struct{}, len(items))
	totalOperations := 0
	// Each item is decoded independently while aggregate operation work remains
	// capped across the complete request.
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]interface{})
		// Rejecting malformed coercion uniformly keeps raw input out of errors.
		if !ok {
			return nil, errAppScaffoldRequirementsUnavailable
		}
		selection, err := decodeAppScaffoldSelection(item)
		// Invalid field values fail the complete batch before authorization reads.
		if err != nil {
			return nil, err
		}
		// Service keys are unique in SDK/MCP config maps; enforcing that invariant
		// makes each output service label unambiguous.
		if _, exists := seenServices[selection.Service]; exists {
			return nil, errAppScaffoldRequirementsUnavailable
		}
		seenServices[selection.Service] = struct{}{}
		totalOperations += len(selection.Operations)
		// The aggregate bound prevents many individually valid selections from
		// producing an oversized JSONB contract query.
		if totalOperations > maxAppScaffoldOperationsTotal {
			return nil, errAppScaffoldRequirementsUnavailable
		}
		selections = append(selections, selection)
	}
	return selections, nil
}

// decodeAppScaffoldSelection keeps scalar and operation validation separate
// from the orchestration path so its decision complexity stays reviewable.
func decodeAppScaffoldSelection(item map[string]interface{}) (appScaffoldSelection, error) {
	service, serviceOK := item["service"].(string)
	version, versionOK := item["version"].(string)
	selectAll, selectAllOK := item["select_all"].(bool)
	// Stable labels must be canonical and bounded because they are later joined
	// as data, not interpreted as SQL or logged.
	if !serviceOK || !validAppScaffoldLabel(service, maxAppScaffoldServiceKeyBytes) {
		return appScaffoldSelection{}, errAppScaffoldRequirementsUnavailable
	}
	// Exact immutable versions use the same closed scalar rules as service keys.
	if !versionOK || !validAppScaffoldLabel(version, maxAppScaffoldVersionBytes) {
		return appScaffoldSelection{}, errAppScaffoldRequirementsUnavailable
	}
	// Boolean coercion is required so omitted select_all cannot silently broaden.
	if !selectAllOK {
		return appScaffoldSelection{}, errAppScaffoldRequirementsUnavailable
	}
	operations, err := decodeAppScaffoldOperations(item["operations"])
	// Operation decoding owns deduplication and per-selection bounds.
	if err != nil {
		return appScaffoldSelection{}, err
	}
	return appScaffoldSelection{Service: service, Version: version, Operations: operations, SelectAll: selectAll}, nil
}

// decodeAppScaffoldOperations canonicalizes exact operation names while
// preserving an omitted list as an intentionally empty selection.
func decodeAppScaffoldOperations(raw any) ([]string, error) {
	// Nullable GraphQL operations permit callers to express select_all without
	// allocating a redundant empty list.
	if raw == nil {
		return []string{}, nil
	}
	items, ok := raw.([]interface{})
	// Per-service bounds make the endpoint batch predictable before JSON encoding.
	if !ok || len(items) > maxAppScaffoldOperationsPerSelection {
		return nil, errAppScaffoldRequirementsUnavailable
	}
	operations := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	// Deduplication prevents repeated names from distorting missing-operation checks.
	for _, rawItem := range items {
		operation, ok := rawItem.(string)
		// Runtime operation IDs share the REST boundary's byte cap and reject
		// control characters before becoming database query values.
		if !ok || !validAppScaffoldLabel(operation, maxRESTOperationBytes) {
			return nil, errAppScaffoldRequirementsUnavailable
		}
		// Duplicate operation names carry no additional selection authority.
		if _, exists := seen[operation]; exists {
			continue
		}
		seen[operation] = struct{}{}
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	return operations, nil
}

// validAppScaffoldLabel admits only exact, display-safe scalar values so
// canonicalization never changes the service or operation being requested.
func validAppScaffoldLabel(value string, maxBytes int) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= maxBytes && !strings.ContainsAny(value, "\r\n\x00")
}

// appScaffoldOperationCount returns a safe aggregate for diagnostics without
// exposing which operations or services were requested.
func appScaffoldOperationCount(selections []appScaffoldSelection) int {
	total := 0
	// Counting already-bounded selections cannot expand memory or query work.
	for _, selection := range selections {
		total += len(selection.Operations)
	}
	return total
}

// loadAppScaffoldRequirements performs one authorized identity lookup and one
// bounded read for each existing contract/policy projection.
func loadAppScaffoldRequirements(ctx context.Context, repository appScaffoldRequirementStore, authorized accesscontrol.AuthorizedScope, selections []appScaffoldSelection) ([]appScaffoldRequirement, error) {
	resolved, err := repository.ResolveAuthorizedAppScaffoldSelections(ctx, authorized, appScaffoldSelectionRefs(selections))
	// Exact resolution is atomic; callers never continue with a partial service set.
	if err != nil || len(resolved) != len(selections) {
		return nil, errAppScaffoldRequirementsUnavailable
	}
	metadataRefs, policyRefs, endpointSelections, err := appScaffoldBatchInputs(selections, resolved)
	// Invalid store output indicates an adapter contract breach, not a client miss.
	if err != nil {
		return nil, err
	}
	metadata, err := repository.ListServiceContractMetadata(ctx, metadataRefs)
	// Missing or incompatible metadata invalidates the immutable batch.
	if err != nil {
		return nil, err
	}
	endpoints, err := repository.ListServiceContractEndpointsForSelections(ctx, endpointSelections, nil)
	// Endpoint snapshots are fetched in one set query rather than per selection.
	if err != nil {
		return nil, err
	}
	overrides, err := repository.GetEffectiveWorkspaceExecutionPolicyOverrides(ctx, policyRefs)
	// Policy values must be applied before projecting unresolved variable names.
	if err != nil {
		return nil, err
	}
	grouped, err := groupAppScaffoldEndpoints(selections, endpoints)
	// Explicit selections must resolve every requested operation before prompting.
	if err != nil {
		return nil, err
	}
	return collectAppScaffoldRequirements(selections, resolved, metadata, overrides, grouped)
}

// appScaffoldSelectionRefs preserves input indexes across the authorized
// set-based identity lookup.
func appScaffoldSelectionRefs(selections []appScaffoldSelection) []store.AppScaffoldSelectionRef {
	refs := make([]store.AppScaffoldSelectionRef, len(selections))
	// Positional identity links the returned UUIDs to the original service key.
	for index, selection := range selections {
		refs[index] = store.AppScaffoldSelectionRef{SelectionIndex: index, ServiceKey: selection.Service, Version: selection.Version}
	}
	return refs
}

// appScaffoldBatchInputs validates returned indexes while preparing parallel
// metadata, endpoint, and effective-policy batch requests.
func appScaffoldBatchInputs(selections []appScaffoldSelection, resolved []store.AppScaffoldResolvedSelection) ([]store.ServiceContractMetadataRef, []store.WorkspaceExecutionPolicyRef, []store.ServiceContractEndpointSelection, error) {
	metadataRefs := make([]store.ServiceContractMetadataRef, len(selections))
	policyRefs := make([]store.WorkspaceExecutionPolicyRef, len(selections))
	endpointSelections := make([]store.ServiceContractEndpointSelection, len(selections))
	seen := make(map[int]struct{}, len(selections))
	// The store may return rows in any order, so the declared selection index is
	// the only safe join key for the three bounded reads.
	for _, item := range resolved {
		// Out-of-range or repeated indexes could associate a contract with the wrong key.
		if item.SelectionIndex < 0 || item.SelectionIndex >= len(selections) {
			return nil, nil, nil, errAppScaffoldRequirementsUnavailable
		}
		// Duplicate resolved rows violate the one-service-per-selection invariant.
		if _, exists := seen[item.SelectionIndex]; exists {
			return nil, nil, nil, errAppScaffoldRequirementsUnavailable
		}
		seen[item.SelectionIndex] = struct{}{}
		selection := selections[item.SelectionIndex]
		operationNames := appScaffoldExplicitOperationNames(selection)
		metadataRefs[item.SelectionIndex] = store.ServiceContractMetadataRef{ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID}
		policyRefs[item.SelectionIndex] = store.WorkspaceExecutionPolicyRef{ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID}
		endpointSelections[item.SelectionIndex] = store.ServiceContractEndpointSelection{
			SelectionIndex: item.SelectionIndex, ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID,
			SelectAll: selection.SelectAll, OperationNames: operationNames, EndpointNames: operationNames,
		}
	}
	return metadataRefs, policyRefs, endpointSelections, nil
}

// appScaffoldExplicitOperationNames preserves select_all as the dominant
// selection mode even when a caller also carries a stale operation list.
func appScaffoldExplicitOperationNames(selection appScaffoldSelection) []string {
	// Select-all must not be narrowed by the endpoint-name intersection clause.
	if selection.SelectAll {
		return nil
	}
	return selection.Operations
}

// groupAppScaffoldEndpoints groups only SQL-selected rows and verifies that an
// explicit operation list was not partially missing from its snapshot.
func groupAppScaffoldEndpoints(selections []appScaffoldSelection, matches []store.ServiceContractEndpointMatch) (map[int][]fusedobject.Endpoint, error) {
	grouped := make(map[int][]fusedobject.Endpoint, len(selections))
	// Matches are already filtered in PostgreSQL; Go only joins them by bounded index.
	for _, match := range matches {
		// A foreign index cannot be allowed to influence another service's requirements.
		if match.SelectionIndex < 0 || match.SelectionIndex >= len(selections) {
			return nil, errAppScaffoldRequirementsUnavailable
		}
		grouped[match.SelectionIndex] = append(grouped[match.SelectionIndex], match.Endpoint)
	}
	// Exact operation selections must not silently downgrade to a partial result.
	for index, selection := range selections {
		// Select-all cardinality is snapshot-owned, while named selection has an exact expected count.
		if !selection.SelectAll && len(grouped[index]) != len(selection.Operations) {
			return nil, errAppScaffoldRequirementsUnavailable
		}
	}
	return grouped, nil
}

// collectAppScaffoldRequirements applies the shared runtime projector to each
// exact service/version and emits a deterministic service/name pair set.
func collectAppScaffoldRequirements(selections []appScaffoldSelection, resolved []store.AppScaffoldResolvedSelection, metadata map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, overrides map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride, grouped map[int][]fusedobject.Endpoint) ([]appScaffoldRequirement, error) {
	requirements := make([]appScaffoldRequirement, 0)
	// Each selection uses only its exact metadata, policy, and SQL-selected endpoints.
	for _, item := range resolved {
		metadataRef := store.ServiceContractMetadataRef{ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID}
		policyRef := store.WorkspaceExecutionPolicyRef{ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID}
		names, err := sandbox.AppScaffoldRequiredServerVariables(metadata[metadataRef], grouped[item.SelectionIndex], overrides[policyRef])
		// Runtime routing errors fail the full initialization instead of generating incomplete code.
		if err != nil {
			return nil, err
		}
		// Variable names are safe GraphQL output but remain absent from telemetry and errors.
		for _, name := range names {
			requirements = append(requirements, appScaffoldRequirement{Service: selections[item.SelectionIndex].Service, Variable: name})
		}
	}
	// Sorting provides stable output independent of SQL row order.
	sort.Slice(requirements, func(left, right int) bool {
		if requirements[left].Service == requirements[right].Service {
			return requirements[left].Variable < requirements[right].Variable
		}
		return requirements[left].Service < requirements[right].Service
	})
	return requirements, nil
}

// projectAppScaffoldRequirements uses explicit field maps so GraphQL naming
// never depends on Go reflection conventions.
func projectAppScaffoldRequirements(requirements []appScaffoldRequirement) []map[string]interface{} {
	projected := make([]map[string]interface{}, 0, len(requirements))
	// Projection is bounded by unique server variables in selected snapshots.
	for _, requirement := range requirements {
		projected = append(projected, map[string]interface{}{"service": requirement.Service, "variable": requirement.Variable})
	}
	return projected
}
