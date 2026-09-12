package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
)

var appFamilyURLsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppFamilyTransportURLs",
	Fields: graphql.Fields{
		"streamable_http": &graphql.Field{Type: graphql.String},
		"sse":             &graphql.Field{Type: graphql.String},
	},
})

var appFamilySummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppFamilySummary",
	Fields: graphql.Fields{
		"app_family_id":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"kind":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"target_language":   &graphql.Field{Type: graphql.String},
		"version_count":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"latest_version":    &graphql.Field{Type: graphql.String},
		"latest_version_id": &graphql.Field{Type: graphql.String},
		"latest_status":     &graphql.Field{Type: graphql.String},
		"latest_created_at": &graphql.Field{Type: graphql.String},
		"downloads":         &graphql.Field{Type: graphql.String},
		"stable_version":    &graphql.Field{Type: graphql.String},
		"stable_version_id": &graphql.Field{Type: graphql.String},
		"default_transport": &graphql.Field{Type: graphql.String},
		"transport_urls":    &graphql.Field{Type: appFamilyURLsGraphQLType},
	},
})

var appFamilyPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppFamilyPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appFamilySummaryGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

// appFamiliesGraphQLField exposes an additive application catalogue while preserving existing version-level detail queries.
func appFamiliesGraphQLField(s store.Store, downloadClient sandbox.SDKPackageDownloadCountClient) *graphql.Field {
	return &graphql.Field{Type: appFamilyPageGraphQLType, Args: graphql.FieldConfigArgument{
		"kind":   &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		"search": &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		"limit":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
		"offset": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		// Reuse the same actor and authorized family scope as existing application reads.
	}, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		_, actor, authorized, err := authorizedAppCatalog(p, s)
		// Authentication and authorization errors must precede any catalogue reads.
		if err != nil {
			return nil, err
		}
		repository, ok := s.(store.AppFamilyCatalogRepository)
		// Never substitute client-style grouping when the authoritative family projection is unavailable.
		if !ok {
			return nil, errors.New("app family catalogue is unavailable")
		}
		limit, offset := boundedAppPage(p.Args)
		items, total, err := repository.ListAuthorizedAppFamilies(p.Context, actor.accountID, authorized,
			strings.TrimSpace(fmt.Sprint(p.Args["kind"])), strings.TrimSpace(fmt.Sprint(p.Args["search"])), limit, offset)
		// A failed or incomplete database read cannot establish a successful family page.
		if err != nil {
			return nil, err
		}
		counts, available := appDownloadCounts(p, downloadClient, appFamilyLatestApps(items))
		projected := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			projected = append(projected, appFamilySummaryFields(requestFromContext(p.Context), item, counts, available))
		}
		return map[string]interface{}{"items": projected, "total": total}, nil
	}}
}

// appFamilyLatestApps adapts family catalogue metadata for the shared optional download-count batch.
func appFamilyLatestApps(items []store.AppFamilyCatalogItem) []store.AppCatalogItem {
	apps := make([]store.AppCatalogItem, 0, len(items))
	for _, item := range items {
		// Retained families without live versions have no package identity to send to Registry.
		if item.LatestAppID == uuid.Nil {
			continue
		}
		apps = append(apps, store.AppCatalogItem{AppID: item.LatestAppID, Kind: item.Kind})
	}
	return apps
}

// appFamilySummaryFields exposes logical identity, latest presentation metadata, and explicit MCP promotion state.
func appFamilySummaryFields(r *http.Request, item store.AppFamilyCatalogItem, counts map[uuid.UUID]int64, countsAvailable bool) map[string]interface{} {
	result := map[string]interface{}{
		"app_family_id": item.AppFamilyID.String(), "name": item.Name, "kind": string(item.Kind),
		"target_language": item.TargetLanguage, "version_count": item.VersionCount,
	}
	// A retained family can have no live version, so latest fields appear only with an exact immutable identity.
	if item.LatestAppID != uuid.Nil {
		result["latest_version"], result["latest_version_id"] = item.LatestVersion, item.LatestAppID.String()
		result["latest_status"] = string(item.LatestStatus)
		// Creation time is available for every persisted app row selected as latest.
		if item.LatestCreatedAt != nil {
			result["latest_created_at"] = item.LatestCreatedAt.Format(mcpGraphQLTimeFormat)
		}
		// Registry analytics are optional and only apply to generated SDK packages.
		if item.Kind == store.AppKindSDK && countsAvailable {
			result["downloads"] = strconv.FormatInt(counts[item.LatestAppID], 10)
		}
	}
	// SDKs have no implicit current version; an unpromoted MCP must not borrow a sibling's route.
	if item.Kind == store.AppKindMCP && item.StableAppID != uuid.Nil {
		urls := mcpTransportURLsForApp(r, item.AppFamilyID, item.StableAppID, item.StableAppID)
		result["stable_version"], result["stable_version_id"] = item.StableVersion, item.StableAppID.String()
		result["default_transport"] = mcpDefaultTransport
		result["transport_urls"] = map[string]interface{}{"streamable_http": urls.StreamableHTTP, "sse": urls.SSE}
	}
	return result
}
