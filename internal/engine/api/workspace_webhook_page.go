package api

import (
	"context"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/graphql-go/graphql"
)

var workspaceWebhookPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceWebhookPage",
	Fields: graphql.Fields{
		"total": &graphql.Field{Type: graphql.Int},
		"items": &graphql.Field{Type: graphql.NewList(workspaceWebhookGraphQLType)},
	},
})

// workspaceWebhookPageGraphQLField serves local ingress registrations without catalogue or per-service fan-out.
func workspaceWebhookPageGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: workspaceWebhookPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"limit":      &graphql.ArgumentConfig{Type: graphql.Int},
			"offset":     &graphql.ArgumentConfig{Type: graphql.Int},
			"service_id": &graphql.ArgumentConfig{Type: graphql.String},
			"search":     &graphql.ArgumentConfig{Type: graphql.String},
		},
		// Bound database latency and retain the same service-read scope as individual registration queries.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, cancel := context.WithTimeout(p.Context, 10*time.Second)
			defer cancel()
			scope, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
			// Never query registration metadata before the actor's resource boundary is known.
			if err != nil {
				return nil, err
			}
			serviceID, err := optionalGraphQLUUIDArg(p, "service_id")
			// Reject malformed filters before querying; the filter never replaces the authorized scope.
			if err != nil {
				return nil, err
			}
			search, _ := p.Args["search"].(string)
			filter := store.WorkspaceWebhookFilter{ServiceID: serviceID, Search: strings.TrimSpace(search)}
			limit, offset := bucketPageArgs(p)
			rows, total, err := s.ListAuthorizedWorkspaceWebhookPage(ctx, scope, filter, limit, offset)
			// Fail the page explicitly instead of showing a success-shaped empty result.
			if err != nil {
				return nil, err
			}
			items := make([]map[string]interface{}, 0, len(rows))
			// Reuse URL and credential-metadata projection so batch reads do not widen disclosure.
			for _, row := range rows {
				item := projectGraphQLWorkspaceWebhooks([]store.WorkspaceWebhook{row.WorkspaceWebhook}, ctx)[0]
				item["service_id"], item["service_name"], item["service_ref"] = row.ServiceID.String(), row.ServiceName, row.ServiceRef
				items = append(items, item)
			}
			return map[string]interface{}{"items": items, "total": total}, nil
		},
	}
}
