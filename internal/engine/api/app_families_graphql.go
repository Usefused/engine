package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

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

// appFamiliesGraphQLField exposes an additive application catalogue while preserving existing version-level UI queries.
func appFamiliesGraphQLField(s store.Store) *graphql.Field {
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
		projected := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			projected = append(projected, appFamilySummaryFields(requestFromContext(p.Context), item))
		}
		return map[string]interface{}{"items": projected, "total": total}, nil
	}}
}

// appFamilySummaryFields exposes only logical identity and Engine's explicitly promoted MCP transport.
func appFamilySummaryFields(r *http.Request, item store.AppFamilyCatalogItem) map[string]interface{} {
	result := map[string]interface{}{
		"app_family_id": item.AppFamilyID.String(), "name": item.Name, "kind": string(item.Kind),
		"target_language": item.TargetLanguage, "version_count": item.VersionCount,
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
