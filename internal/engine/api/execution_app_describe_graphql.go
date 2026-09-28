package api

import (
	"context"
	"errors"
	"sort"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var errExecutionBuildSelectionsUnavailable = errors.New("execution build selections unavailable")

type executionBuildSelectionStore interface {
	store.AppScaffoldSelectionStore
	store.ServiceContractEndpointSelectionBatchStore
}

var executionBuildSelectionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ExecutionBuildSelection",
	Fields: graphql.Fields{
		"service":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"operation":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"serviceId":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"serviceVersionId": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"endpointId":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

// executionBuildSelectionsGraphQLField returns exact workspace operation IDs
// for a reviewed compiler spec without exposing provider credentials or bodies.
func executionBuildSelectionsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(executionBuildSelectionGraphQLType))),
		Args: graphql.FieldConfigArgument{
			"selections": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appScaffoldSelectionGraphQLInput)))},
		},
		// Keep GraphQL adapter work outside the schema declaration so the
		// authorization and batch-read path remains independently testable.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return resolveExecutionBuildSelections(p, s) },
	}
}

// resolveExecutionBuildSelections records a count-only authoring span while
// preserving the same service.read scope used by the SQL identity lookup.
func resolveExecutionBuildSelections(p graphql.ResolveParams, s store.Store) (interface{}, error) {
	ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.execution_build_selections")
	defer span.End()
	selections, err := decodeExecutionBuildSelections(p.Args["selections"])
	// Bounds and exact operation names must precede authorization and SQL work.
	if err != nil {
		span.SetStatus(codes.Error, "execution build selections unavailable")
		return nil, err
	}
	span.SetAttributes(attribute.Int("execution.build_operation_count", appScaffoldOperationCount(selections)))
	repository, ok := s.(executionBuildSelectionStore)
	// A partial store cannot safely substitute per-operation lookups.
	if !ok {
		span.SetStatus(codes.Error, "execution build selections unavailable")
		return nil, errExecutionBuildSelectionsUnavailable
	}
	authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
	// Unauthenticated discovery must not reveal workspace operation identities.
	if err != nil {
		span.SetStatus(codes.Error, "execution build selections unavailable")
		return nil, err
	}
	result, err := loadExecutionBuildSelections(ctx, repository, authorized, selections)
	// Missing or inaccessible selections share one non-enumerating response.
	if err != nil {
		span.SetStatus(codes.Error, "execution build selections unavailable")
		return nil, errExecutionBuildSelectionsUnavailable
	}
	return result, nil
}

// decodeExecutionBuildSelections limits authored code to a finite, exact
// operation set that the Engine can independently verify on bundle attach.
func decodeExecutionBuildSelections(raw any) ([]appScaffoldSelection, error) {
	selections, err := decodeAppScaffoldSelections(raw)
	if err != nil {
		return nil, errExecutionBuildSelectionsUnavailable
	}
	count := 0
	for _, selection := range selections {
		// A moving select-all catalogue cannot be pinned into immutable bundle IDs.
		if selection.SelectAll || len(selection.Operations) == 0 {
			return nil, errExecutionBuildSelectionsUnavailable
		}
		count += len(selection.Operations)
	}
	// The compiler manifest and Engine attach route share this bound.
	if count > maxExecutionAppSelectedOperations {
		return nil, errExecutionBuildSelectionsUnavailable
	}
	return selections, nil
}

// loadExecutionBuildSelections resolves service identity and endpoints in two
// set-based reads, then returns only the IDs required by the local compiler.
func loadExecutionBuildSelections(ctx context.Context, repository executionBuildSelectionStore, authorized accesscontrol.AuthorizedScope, selections []appScaffoldSelection) ([]map[string]interface{}, error) {
	resolved, err := repository.ResolveAuthorizedAppScaffoldSelections(ctx, authorized, appScaffoldSelectionRefs(selections))
	// Partial workspace resolution cannot produce a deployable build spec.
	if err != nil || len(resolved) != len(selections) {
		return nil, errExecutionBuildSelectionsUnavailable
	}
	_, _, endpointSelections, err := appScaffoldBatchInputs(selections, resolved)
	if err != nil {
		return nil, errExecutionBuildSelectionsUnavailable
	}
	matches, err := repository.ListServiceContractEndpointsForSelections(ctx, endpointSelections, nil)
	if err != nil {
		return nil, errExecutionBuildSelectionsUnavailable
	}
	grouped, err := groupAppScaffoldEndpoints(selections, matches)
	if err != nil {
		return nil, errExecutionBuildSelectionsUnavailable
	}
	return projectExecutionBuildSelections(selections, resolved, grouped)
}

// projectExecutionBuildSelections binds each SQL-selected endpoint to its exact
// workspace version while retaining deterministic authored service order.
func projectExecutionBuildSelections(selections []appScaffoldSelection, resolved []store.AppScaffoldResolvedSelection, grouped map[int][]fusedobject.Endpoint) ([]map[string]interface{}, error) {
	byIndex, err := indexExecutionBuildSelections(selections, resolved)
	// A malformed identity batch cannot be bound to endpoint rows safely.
	if err != nil {
		return nil, err
	}
	result := make([]map[string]interface{}, 0, appScaffoldOperationCount(selections))
	for index, selected := range selections {
		identity, exists := byIndex[index]
		// Every authored service must have one exact workspace version.
		if !exists {
			return nil, errExecutionBuildSelectionsUnavailable
		}
		endpoints := grouped[index]
		// SQL row order is not stable, so operation output follows the compiler's canonical name order.
		sort.Slice(endpoints, func(left, right int) bool { return endpoints[left].Name < endpoints[right].Name })
		for _, endpoint := range endpoints {
			// An unpinned contract endpoint cannot be used as immutable code authority.
			if endpoint.ID == uuid.Nil {
				return nil, errExecutionBuildSelectionsUnavailable
			}
			result = append(result, map[string]interface{}{
				"service": selected.Service, "operation": endpoint.Name,
				"serviceId": identity.ServiceID.String(), "serviceVersionId": identity.ServiceVersionID.String(), "endpointId": endpoint.ID.String(),
			})
		}
	}
	return result, nil
}

// indexExecutionBuildSelections rejects malformed store rows before their
// IDs can be paired with SQL-selected endpoint snapshots.
func indexExecutionBuildSelections(selections []appScaffoldSelection, resolved []store.AppScaffoldResolvedSelection) (map[int]store.AppScaffoldResolvedSelection, error) {
	byIndex := make(map[int]store.AppScaffoldResolvedSelection, len(resolved))
	for _, selection := range resolved {
		// Duplicate or foreign result indexes could attach code to another service.
		if selection.SelectionIndex < 0 || selection.SelectionIndex >= len(selections) || selection.ServiceID == uuid.Nil || selection.ServiceVersionID == uuid.Nil {
			return nil, errExecutionBuildSelectionsUnavailable
		}
		// A repeated index could quietly replace the reviewed service identity.
		if _, exists := byIndex[selection.SelectionIndex]; exists {
			return nil, errExecutionBuildSelectionsUnavailable
		}
		byIndex[selection.SelectionIndex] = selection
	}
	return byIndex, nil
}
