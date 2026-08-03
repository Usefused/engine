package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var artifactSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactSummary",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"version":    &graphql.Field{Type: graphql.String},
		"kind":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"active":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"created_at": &graphql.Field{Type: graphql.String},
	},
})

var artifactSummaryPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactSummaryPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(artifactSummaryGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var artifactServiceSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactServiceSummary",
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

func artifactsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: artifactSummaryPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"kind":   &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
			"limit":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repository, ok := s.(store.ArtifactPageRepository)
			if !ok {
				return nil, errors.New("artifact pages are unavailable")
			}
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			authorized, err := graphQLAuthorizedScope(p.Context, accesscontrol.PermissionArtifactRead, accesscontrol.ResourceArtifact)
			if err != nil {
				return nil, err
			}
			kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(p.Args["kind"])))
			limit, _ := p.Args["limit"].(int)
			offset, _ := p.Args["offset"].(int)
			// Bound direct GraphQL callers as well as the CLI so one read cannot
			// turn the human-friendly list endpoint into an unbounded DB scan.
			if limit <= 0 || limit > 100 {
				limit = 20
			}
			if offset < 0 {
				offset = 0
			}
			scopes, total, err := repository.ListAuthorizedArtifactScopesByAccount(p.Context, actor.accountID, authorized, kind, limit, offset)
			if err != nil {
				return nil, err
			}
			items := make([]map[string]interface{}, 0, len(scopes))
			for _, scope := range scopes {
				items = append(items, artifactSummaryFields(scope))
			}
			return map[string]interface{}{"items": items, "total": total}, nil
		},
	}
}

func artifactGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: artifactSummaryGraphQLType,
		Args: graphql.FieldConfigArgument{
			"reference": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"kind":      &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			value := strings.TrimSpace(fmt.Sprint(p.Args["reference"]))
			artifactID, err := resolveVisibleResourceReference(p, s, store.ReferenceArtifact, value, referenceArtifactKind(p, store.ReferenceArtifact))
			if err != nil {
				return nil, err
			}
			scope, err := s.GetArtifactScope(p.Context, artifactID)
			if err != nil {
				return nil, errors.New("artifact was not found")
			}
			return artifactSummaryFields(*scope), nil
		},
	}
}

func artifactServicesGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(artifactServiceSummaryGraphQLType))),
		Args: graphql.FieldConfigArgument{
			"reference": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"kind":      &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repository, ok := s.(store.ArtifactServiceRepository)
			if !ok {
				return nil, errors.New("artifact service summaries are unavailable")
			}
			value := strings.TrimSpace(fmt.Sprint(p.Args["reference"]))
			artifactID, err := resolveVisibleResourceReference(p, s, store.ReferenceArtifact, value, referenceArtifactKind(p, store.ReferenceArtifact))
			if err != nil {
				return nil, err
			}
			services, err := repository.ListArtifactServiceSummaries(p.Context, artifactID)
			if err != nil {
				return nil, err
			}
			items := make([]map[string]interface{}, 0, len(services))
			for _, service := range services {
				items = append(items, artifactServiceSummaryFields(service))
			}
			return items, nil
		},
	}
}

func artifactServiceSummaryFields(service store.ArtifactServiceSummary) map[string]interface{} {
	return map[string]interface{}{
		"service_id": service.ServiceID.String(), "service_slug": service.ServiceSlug, "service_name": service.ServiceName,
		"version": service.Version, "select_all": service.SelectAll, "endpoint_count": service.EndpointCount, "webhook_count": service.WebhookCount,
	}
}

func artifactSummaryFields(scope store.ArtifactScope) map[string]interface{} {
	return map[string]interface{}{
		"id": scope.ArtifactID.String(), "name": scope.Name, "version": scope.Version,
		"kind": scope.Kind, "active": scope.DeactivatedAt == nil, "created_at": scope.CreatedAt.Format(mcpGraphQLTimeFormat),
	}
}
