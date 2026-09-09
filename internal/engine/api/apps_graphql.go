package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

var appSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppSummary",
	Fields: graphql.Fields{
		"app_family_id":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"app_id":                  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":                    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"description":             &graphql.Field{Type: graphql.String},
		"version":                 &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"kind":                    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"status":                  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"created_at":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"target_language":         &graphql.Field{Type: graphql.String},
		"generator_version":       &graphql.Field{Type: graphql.String},
		"downloads":               &graphql.Field{Type: graphql.String},
		"readme":                  &graphql.Field{Type: graphql.String},
		"default_transport":       &graphql.Field{Type: graphql.String},
		"transport_urls":          &graphql.Field{Type: mcpTransportURLsGraphQLType},
		"stable":                  &graphql.Field{Type: graphql.Boolean},
		"stable_version_id":       &graphql.Field{Type: graphql.String},
		"selections":              &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appSelectionGraphQLType)))},
		"planned_deactivation_at": &graphql.Field{Type: graphql.String},
	},
})

var appSelectionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppSelection",
	Fields: graphql.Fields{
		"service_id":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"service_version_id": &graphql.Field{Type: graphql.String},
		"schema_version":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"endpoint_ids":       &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"operation_names":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"webhook_ids":        &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"webhook_names":      &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"select_all":         &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"webhook_select_all": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"auth_type":          &graphql.Field{Type: graphql.String},
		"auth_name":          &graphql.Field{Type: graphql.String},
		"auth_ref":           &graphql.Field{Type: graphql.String},
		"required_auth":      &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appRequiredAuthGraphQLType)))},
		"connect_scopes":     &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"injections":         &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appInjectionGraphQLType)))},
	},
})

var appRequiredAuthGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppRequiredAuth",
	Fields: graphql.Fields{
		"auth_type":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"auth_name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"basic_password_mode": &graphql.Field{Type: graphql.String},
	},
})

var appInjectionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppInjection",
	Fields: graphql.Fields{
		"location": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"value":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"mode":     &graphql.Field{Type: graphql.String},
	},
})

var appSummaryPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppSummaryPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appSummaryGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var appServiceSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppServiceSummary",
	Fields: graphql.Fields{
		"service_id":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"service_slug":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"service_name":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"version":        &graphql.Field{Type: graphql.String},
		"select_all":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"endpoint_count": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"webhook_count":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

const (
	appOperationKindPhysical = "physical"
	appOperationKindUnified  = "unified"
)

// mcpAppOperation is one exact callable name in an immutable MCP version.
type mcpAppOperation struct {
	OperationID      string
	Kind             string
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
}

// mcpAppOperationCatalogue binds operation discovery to one exact MCP version.
type mcpAppOperationCatalogue struct {
	AppFamilyID uuid.UUID
	AppID       uuid.UUID
	Name        string
	Version     string
	Operations  []mcpAppOperation
}

var mcpAppOperationGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPAppOperation",
	Fields: graphql.Fields{
		"operation_id":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"kind":               &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"service_id":         &graphql.Field{Type: graphql.String},
		"service_version_id": &graphql.Field{Type: graphql.String},
	},
})

var mcpAppOperationCatalogueGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPAppOperationCatalogue",
	Fields: graphql.Fields{
		"mcp_id":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"version_id": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"version":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"operations": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(mcpAppOperationGraphQLType)))},
		"total":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

// appsGraphQLField lists authorized app versions and enriches selected SDK
// download counts through one optional Registry batch.
func appsGraphQLField(s store.Store, downloadClient sandbox.SDKPackageDownloadCountClient) *graphql.Field {
	return &graphql.Field{Type: appSummaryPageGraphQLType, Args: graphql.FieldConfigArgument{
		"kind":    &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		"search":  &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		"version": &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		"limit":   &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
		"offset":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
	}, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		repository, actor, authorized, err := authorizedAppCatalog(p, s)
		if err != nil {
			return nil, err
		}
		limit, offset := boundedAppPage(p.Args)
		items, total, err := repository.ListAuthorizedAppsByAccount(
			p.Context, actor.accountID, authorized, strings.TrimSpace(fmt.Sprint(p.Args["kind"])),
			strings.TrimSpace(fmt.Sprint(p.Args["search"])), strings.TrimSpace(fmt.Sprint(p.Args["version"])), limit, offset,
		)
		if err != nil {
			return nil, err
		}
		counts, available := appDownloadCounts(p, downloadClient, items)
		return map[string]interface{}{"items": appSummaryFields(requestFromContext(p.Context), items, counts, available), "total": total}, nil
	}}
}

// appGraphQLField reads one exact authorized app and its optional durable SDK
// download count without changing lifecycle ownership.
func appGraphQLField(s store.Store, downloadClient sandbox.SDKPackageDownloadCountClient) *graphql.Field {
	return &graphql.Field{Type: appSummaryGraphQLType, Args: graphql.FieldConfigArgument{
		"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		repository, actor, authorized, err := authorizedAppCatalog(p, s)
		if err != nil {
			return nil, err
		}
		appID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(p.Args["app_id"])))
		if err != nil {
			return nil, errors.New("app was not found")
		}
		item, err := repository.GetAuthorizedApp(p.Context, actor.accountID, appID, authorized)
		if err != nil {
			return nil, errors.New("app was not found")
		}
		counts, available := appDownloadCounts(p, downloadClient, []store.AppCatalogItem{*item})
		return appSummaryField(requestFromContext(p.Context), *item, counts, available), nil
	}}
}

// appVersionsGraphQLField lists one authorized family and batches any selected
// SDK download counts across its immutable app versions.
func appVersionsGraphQLField(s store.Store, downloadClient sandbox.SDKPackageDownloadCountClient) *graphql.Field {
	return &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appSummaryGraphQLType))), Args: graphql.FieldConfigArgument{
		"app_family_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		repository, actor, authorized, err := authorizedAppCatalog(p, s)
		if err != nil {
			return nil, err
		}
		familyID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(p.Args["app_family_id"])))
		if err != nil {
			return nil, errors.New("app family was not found")
		}
		items, err := repository.ListAuthorizedAppsByFamily(p.Context, actor.accountID, familyID, authorized)
		if err != nil {
			return nil, err
		}
		counts, available := appDownloadCounts(p, downloadClient, items)
		return appSummaryFields(requestFromContext(p.Context), items, counts, available), nil
	}}
}

func appServicesGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appServiceSummaryGraphQLType))), Args: graphql.FieldConfigArgument{
		"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		repository, actor, authorized, err := authorizedAppCatalog(p, s)
		if err != nil {
			return nil, err
		}
		appID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(p.Args["app_id"])))
		if err != nil {
			return nil, errors.New("app was not found")
		}
		services, err := repository.ListAuthorizedAppServiceSummaries(p.Context, actor.accountID, appID, authorized)
		if err != nil {
			return nil, err
		}
		projected := make([]map[string]interface{}, 0, len(services))
		for _, service := range services {
			projected = append(projected, appServiceSummaryFields(service))
		}
		return projected, nil
	}}
}

// mcpAppOperationsGraphQLField lists the complete callable catalogue for one authorized immutable MCP version.
func mcpAppOperationsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{Type: mcpAppOperationCatalogueGraphQLType, Args: graphql.FieldConfigArgument{
		"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		repository, actor, authorized, err := authorizedAppCatalog(p, s)
		// Authorization failures must stop before any app-scoped contract data is read.
		if err != nil {
			return nil, err
		}
		appID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(p.Args["app_id"])))
		// Malformed and inaccessible identities remain indistinguishable at the public boundary.
		if err != nil {
			return nil, errors.New("MCP server was not found")
		}
		item, err := repository.GetAuthorizedApp(p.Context, actor.accountID, appID, authorized)
		// Exact app authorization is authoritative even when another version in the family is visible.
		if err != nil || item == nil || item.Kind != store.AppKindMCP {
			return nil, errors.New("MCP server was not found")
		}
		catalogue, err := loadMCPAppOperationCatalogue(p.Context, s, *item)
		// A partial physical or Unified catalogue must never be presented as the complete allowlist.
		if err != nil {
			return nil, err
		}
		return mcpAppOperationCatalogueFields(catalogue), nil
	}}
}

// loadMCPAppOperationCatalogue composes physical selections and Unified descriptors through bounded set-based reads.
func loadMCPAppOperationCatalogue(ctx context.Context, source any, item store.AppCatalogItem) (mcpAppOperationCatalogue, error) {
	endpointStore, ok := source.(store.ServiceContractEndpointSelectionBatchStore)
	// Registry fallback or per-selection reads would make exact-version discovery mutable and N+1.
	if !ok {
		return mcpAppOperationCatalogue{}, errors.New("MCP operation catalogue is unavailable")
	}
	descriptorStore, ok := source.(store.MCPUnifiedDescriptorStore)
	// Unified names must come from the integrity-checked applied plan, never reconstructed private definitions.
	if !ok {
		return mcpAppOperationCatalogue{}, errors.New("MCP Unified operation catalogue is unavailable")
	}
	selections, err := mcpEndpointSelections(item.Selections)
	// Incomplete persisted identities cannot safely select an immutable service contract snapshot.
	if err != nil {
		return mcpAppOperationCatalogue{}, err
	}
	matches, err := endpointStore.ListServiceContractEndpointsForSelections(ctx, selections, nil)
	// One failed batch invalidates the complete physical catalogue.
	if err != nil {
		return mcpAppOperationCatalogue{}, fmt.Errorf("list MCP physical operations: %w", err)
	}
	descriptors, err := descriptorStore.GetMCPUnifiedOperationDescriptors(ctx, item.AppID, true, nil)
	// Descriptor hash or applied-plan failures must not silently erase Unified operations.
	if err != nil {
		return mcpAppOperationCatalogue{}, fmt.Errorf("list MCP Unified operations: %w", err)
	}
	operations, err := mergeMCPAppOperations(item.Selections, matches, descriptors)
	// Duplicate operation IDs make execute routing ambiguous and therefore cannot be advertised as allowed.
	if err != nil {
		return mcpAppOperationCatalogue{}, err
	}
	return mcpAppOperationCatalogue{AppFamilyID: item.AppFamilyID, AppID: item.AppID, Name: item.Name, Version: item.Version, Operations: operations}, nil
}

// mcpEndpointSelections converts immutable public selections into the narrow store batch contract.
func mcpEndpointSelections(selections []models.SDKSelection) ([]store.ServiceContractEndpointSelection, error) {
	requests := make([]store.ServiceContractEndpointSelection, len(selections))
	// Every persisted service selection contributes exactly one row to the set-based snapshot request.
	for index, selection := range selections {
		// New app versions must carry exact v3 service and version identities before any catalogue read.
		if selection.SchemaVersion != models.AppSelectionSchemaVersion || selection.ServiceID == uuid.Nil || selection.ServiceVersionID == uuid.Nil {
			return nil, errors.New("MCP app selection is incomplete")
		}
		requests[index] = store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
			SelectAll: selection.SelectAll, EndpointIDs: selection.EndpointIDs, OperationNames: selection.OperationNames,
		}
	}
	return requests, nil
}

// mergeMCPAppOperations creates one deterministic collision-free physical and Unified catalogue.
func mergeMCPAppOperations(selections []models.SDKSelection, matches []store.ServiceContractEndpointMatch, descriptors *models.SDKUnifiedOperationDescriptors) ([]mcpAppOperation, error) {
	operations := make([]mcpAppOperation, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	// Physical rows inherit exact service provenance from their originating immutable selection.
	for _, match := range matches {
		// A store row outside the submitted selection batch indicates corrupt catalogue identity.
		if match.SelectionIndex < 0 || match.SelectionIndex >= len(selections) {
			return nil, errors.New("MCP physical operation selection is invalid")
		}
		selection := selections[match.SelectionIndex]
		operation := mcpAppOperation{OperationID: match.Endpoint.Name, Kind: appOperationKindPhysical, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
		if err := appendUniqueMCPAppOperation(&operations, seen, operation); err != nil {
			return nil, err
		}
	}
	// A nil descriptor represents an exact applied MCP version with no Unified Operations.
	if descriptors != nil {
		// Unified descriptors contribute only their public invocation identity, never private mappings.
		for _, descriptor := range descriptors.Operations {
			if err := appendUniqueMCPAppOperation(&operations, seen, mcpAppOperation{OperationID: descriptor.Name, Kind: appOperationKindUnified}); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(operations, func(left, right int) bool {
		// Operation ID is the public invocation key; kind is only a deterministic tie-breaker for corrupt duplicate input.
		if operations[left].OperationID == operations[right].OperationID {
			return operations[left].Kind < operations[right].Kind
		}
		return operations[left].OperationID < operations[right].OperationID
	})
	return operations, nil
}

// appendUniqueMCPAppOperation rejects blank or ambiguous public invocation identities.
func appendUniqueMCPAppOperation(operations *[]mcpAppOperation, seen map[string]struct{}, operation mcpAppOperation) error {
	operation.OperationID = strings.TrimSpace(operation.OperationID)
	// Blank names cannot be invoked and must not enter an advertised allowlist.
	if operation.OperationID == "" {
		return errors.New("MCP operation ID is empty")
	}
	_, duplicate := seen[operation.OperationID]
	// Physical/Unified name collisions are runtime-ambiguous and must fail the complete read closed.
	if duplicate {
		return fmt.Errorf("MCP operation ID %q is duplicated", operation.OperationID)
	}
	seen[operation.OperationID] = struct{}{}
	*operations = append(*operations, operation)
	return nil
}

// mcpAppOperationCatalogueFields projects public operation identities without private Unified mappings.
func mcpAppOperationCatalogueFields(catalogue mcpAppOperationCatalogue) map[string]interface{} {
	operations := make([]map[string]interface{}, 0, len(catalogue.Operations))
	// Projection deliberately treats Unified and physical provenance differently at the public boundary.
	for _, operation := range catalogue.Operations {
		serviceID, serviceVersionID := "", ""
		// Physical rows retain exact service provenance while Unified rows intentionally expose no private targets.
		if operation.Kind == appOperationKindPhysical {
			serviceID, serviceVersionID = operation.ServiceID.String(), operation.ServiceVersionID.String()
		}
		operations = append(operations, map[string]interface{}{
			"operation_id": operation.OperationID, "kind": operation.Kind,
			"service_id": serviceID, "service_version_id": serviceVersionID,
		})
	}
	return map[string]interface{}{
		"mcp_id": catalogue.AppFamilyID.String(), "version_id": catalogue.AppID.String(),
		"name": catalogue.Name, "version": catalogue.Version, "operations": operations, "total": len(operations),
	}
}

func authorizedAppCatalog(p graphql.ResolveParams, s store.Store) (store.AppCatalogRepository, mcpGraphQLActor, accesscontrol.AuthorizedScope, error) {
	repository, ok := s.(store.AppCatalogRepository)
	if !ok {
		return nil, mcpGraphQLActor{}, accesscontrol.AuthorizedScope{}, errors.New("app catalogue is unavailable")
	}
	actor, err := actorFromContext(p.Context)
	if err != nil {
		return nil, mcpGraphQLActor{}, accesscontrol.AuthorizedScope{}, err
	}
	authorized, err := graphQLAuthorizedScope(p.Context, accesscontrol.PermissionAppRead, accesscontrol.ResourceApp)
	return repository, actor, authorized, err
}

func boundedAppPage(args map[string]interface{}) (int, int) {
	limit, _ := args["limit"].(int)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := args["offset"].(int)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// appSummaryFields projects a stable GraphQL row for every Engine-owned app.
func appSummaryFields(r *http.Request, items []store.AppCatalogItem, counts map[uuid.UUID]int64, countsAvailable bool) []map[string]interface{} {
	projected := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		projected = append(projected, appSummaryField(r, item, counts, countsAvailable))
	}
	return projected
}

// appSummaryField keeps Registry analytics nullable while preserving exact
// decimal counts beyond JavaScript's safe integer range.
func appSummaryField(r *http.Request, item store.AppCatalogItem, counts map[uuid.UUID]int64, countsAvailable bool) map[string]interface{} {
	planned := ""
	if item.PlannedDeactivationAt != nil {
		planned = item.PlannedDeactivationAt.Format(mcpGraphQLTimeFormat)
	}
	fields := map[string]interface{}{
		"app_family_id": item.AppFamilyID.String(), "app_id": item.AppID.String(),
		"name": item.Name, "description": item.Description, "version": item.Version,
		"kind": item.Kind, "status": item.Status, "created_at": item.CreatedAt.Format(mcpGraphQLTimeFormat),
		"target_language": item.TargetLanguage, "generator_version": item.GeneratorVersion,
		"readme": item.Readme, "selections": appSelectionFields(item), "planned_deactivation_at": planned,
	}
	if item.Kind == store.AppKindSDK && countsAvailable {
		fields["downloads"] = strconv.FormatInt(counts[item.AppID], 10)
	}
	if item.Kind == store.AppKindMCP {
		// Transport discovery belongs to Engine projection so reverse-proxy
		// origins and legacy compatibility never drift across CLI and UI clients.
		transportURLs := mcpTransportURLsForApp(r, item.AppFamilyID, item.AppID, item.StableAppID)
		fields["default_transport"] = mcpDefaultTransport
		fields["transport_urls"] = mcpTransportURLsGraphQLValue(transportURLs)
		fields["stable"] = item.StableAppID == item.AppID
		fields["stable_version_id"] = optionalUUIDString(item.StableAppID)
	}
	return fields
}

// optionalUUIDString keeps absent lifecycle pointers explicit without exposing
// the all-zero storage sentinel through GraphQL.
func optionalUUIDString(id uuid.UUID) string {
	// Zero represents a family with no promoted route after hard deactivation.
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// appDownloadCounts performs one exact Registry batch only when the caller
// selected downloads; Registry outages leave that nullable field unavailable
// without hiding Engine-owned app lifecycle state.
func appDownloadCounts(p graphql.ResolveParams, client sandbox.SDKPackageDownloadCountClient, items []store.AppCatalogItem) (map[uuid.UUID]int64, bool) {
	if client == nil || !appDownloadsRequested(p.Info) {
		return nil, false
	}
	appIDs := sdkAppIDs(items)
	if len(appIDs) == 0 {
		return map[uuid.UUID]int64{}, true
	}
	counts, err := client.FetchSDKPackageDownloadCounts(p.Context, appIDs)
	if err != nil {
		slog.WarnContext(p.Context, "SDK package download counts unavailable", slog.String("failure_code", "registry_count_failed"))
		return nil, false
	}
	return counts, true
}

// sdkAppIDs keeps MCP identities outside Registry's SDK package analytics
// boundary while preserving the requested catalogue order.
func sdkAppIDs(items []store.AppCatalogItem) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		if item.Kind == store.AppKindSDK {
			ids = append(ids, item.AppID)
		}
	}
	return ids
}

// appDownloadsRequested recognizes aliases and fragments so GraphQL never
// creates an unrequested Registry analytics dependency.
func appDownloadsRequested(info graphql.ResolveInfo) bool {
	for _, field := range info.FieldASTs {
		if appSelectionRequestsDownloads(field.SelectionSet, info.Fragments, map[string]bool{}) {
			return true
		}
	}
	return false
}

// appSelectionRequestsDownloads walks one validated selection tree without
// allowing fragment cycles to create unbounded work.
func appSelectionRequestsDownloads(selectionSet *ast.SelectionSet, fragments map[string]ast.Definition, visiting map[string]bool) bool {
	if selectionSet == nil {
		return false
	}
	for _, selection := range selectionSet.Selections {
		if appSelectionRequestsDownload(selection, fragments, visiting) {
			return true
		}
	}
	return false
}

// appSelectionRequestsDownload evaluates one field or fragment edge.
func appSelectionRequestsDownload(selection ast.Selection, fragments map[string]ast.Definition, visiting map[string]bool) bool {
	switch selected := selection.(type) {
	case *ast.Field:
		return selected.Name.Value == "downloads" || appSelectionRequestsDownloads(selected.SelectionSet, fragments, visiting)
	case *ast.InlineFragment:
		return appSelectionRequestsDownloads(selected.SelectionSet, fragments, visiting)
	case *ast.FragmentSpread:
		return appFragmentRequestsDownloads(selected, fragments, visiting)
	default:
		return false
	}
}

// appFragmentRequestsDownloads resolves each named fragment at most once on
// the active recursion path.
func appFragmentRequestsDownloads(spread *ast.FragmentSpread, fragments map[string]ast.Definition, visiting map[string]bool) bool {
	name := spread.Name.Value
	fragment, ok := fragments[name].(*ast.FragmentDefinition)
	if !ok || visiting[name] {
		return false
	}
	visiting[name] = true
	defer delete(visiting, name)
	return appSelectionRequestsDownloads(fragment.SelectionSet, fragments, visiting)
}

// appSelectionFields projects immutable public app policy while keeping internal credential-source IDs private.
func appSelectionFields(item store.AppCatalogItem) []map[string]interface{} {
	selections := make([]map[string]interface{}, 0, len(item.Selections))
	for _, selection := range item.Selections {
		endpointIDs := make([]string, 0, len(selection.EndpointIDs))
		for _, id := range selection.EndpointIDs {
			endpointIDs = append(endpointIDs, id.String())
		}
		webhookIDs := make([]string, 0, len(selection.WebhookIDs))
		for _, id := range selection.WebhookIDs {
			webhookIDs = append(webhookIDs, id.String())
		}
		serviceVersionID := ""
		// A missing version is retained as empty diagnostic data rather than fabricated from current Registry state.
		if selection.ServiceVersionID != uuid.Nil {
			serviceVersionID = selection.ServiceVersionID.String()
		}
		selections = append(selections, map[string]interface{}{
			"service_id": selection.ServiceID.String(), "service_version_id": serviceVersionID,
			"schema_version": selection.SchemaVersion,
			"endpoint_ids":   endpointIDs, "operation_names": nonNilStrings(selection.OperationNames),
			"webhook_ids": webhookIDs, "webhook_names": nonNilStrings(selection.WebhookNames),
			"select_all": selection.SelectAll, "webhook_select_all": selection.WebhookSelectAll,
			"auth_type": selection.AuthType, "auth_name": selection.AuthName, "auth_ref": selection.AuthRef,
			"required_auth":  appRequiredAuthFields(selection.RequiredAuth),
			"connect_scopes": nonNilStrings(selection.ConnectScopes), "injections": appInjectionFields(selection.Injections),
		})
	}
	return selections
}

func appRequiredAuthFields(items []models.SDKRequiredAuth) []map[string]interface{} {
	projected := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		projected = append(projected, map[string]interface{}{
			"auth_type": item.AuthType, "auth_name": item.AuthName,
			"basic_password_mode": string(item.BasicPasswordMode),
		})
	}
	return projected
}

func nonNilStrings(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

func appInjectionFields(items []models.SDKInjectionConfig) []map[string]interface{} {
	projected := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		projected = append(projected, map[string]interface{}{
			"location": item.Location, "name": item.Name, "value": item.Value, "mode": item.Mode,
		})
	}
	return projected
}

func appServiceSummaryFields(service store.AppServiceSummary) map[string]interface{} {
	return map[string]interface{}{
		"service_id": service.ServiceID.String(), "service_slug": service.ServiceSlug,
		"service_name": service.ServiceName, "version": service.Version,
		"select_all": service.SelectAll, "endpoint_count": service.EndpointCount,
		"webhook_count": service.WebhookCount,
	}
}
