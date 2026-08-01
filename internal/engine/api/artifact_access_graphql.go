package api

import (
	"errors"
	"strings"

	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var artifactSelectorResourceTypeGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "ArtifactSelectorResourceType",
	Values: graphql.EnumValueConfigMap{
		"SERVICE": &graphql.EnumValueConfig{Value: accesscontrol.ResourceService},
		"BUCKET":  &graphql.EnumValueConfig{Value: accesscontrol.ResourceBucket},
	},
})

var artifactBuildSelectorGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactBuildSelector",
	Fields: graphql.Fields{
		"resource_type": &graphql.Field{Type: graphql.NewNonNull(artifactSelectorResourceTypeGraphQLEnum)},
		"resource_id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"display_name":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var artifactBuildSelectorPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactBuildSelectorPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(artifactBuildSelectorGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var artifactOwningTeamGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactOwningTeam",
	Fields: graphql.Fields{
		"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"slug": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var artifactOwningTeamPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ArtifactOwningTeamPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(artifactOwningTeamGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

func artifactBuildSelectorsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: artifactBuildSelectorPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"owner_team_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
			"resource_type": &graphql.ArgumentConfig{Type: graphql.NewNonNull(artifactSelectorResourceTypeGraphQLEnum)},
			"search":        &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
			"limit":         &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset":        &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.artifact_build_selectors")
			defer span.End()
			actor, ok := accesscontrol.ActorFromContext(ctx)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			ownerTeamID, err := requiredGraphQLUUIDArg(p, "owner_team_id")
			if err != nil {
				return nil, err
			}
			resourceType, ok := p.Args["resource_type"].(accesscontrol.ResourceType)
			if !ok {
				return nil, errors.New("invalid resource_type")
			}
			limit, offset := bucketPageArgs(p)
			repository, err := artifactAccessRepository(s)
			if err != nil {
				return nil, err
			}
			page, err := repository.ListArtifactBuildSelectors(ctx, store.ArtifactSelectorQuery{
				ActorSubjectID: actor.SubjectID, OwnerTeamID: ownerTeamID, ResourceType: resourceType,
				Search: strings.TrimSpace(graphQLArgString(p, "search")), Limit: limit, Offset: offset,
			})
			if err != nil {
				return nil, errors.New("artifact build selectors unavailable")
			}
			span.SetAttributes(attribute.Int("selector_count", len(page.Items)), attribute.String("resource_type", string(resourceType)))
			return projectArtifactBuildSelectorPage(page), nil
		},
	}
}

func artifactOwningTeamsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: artifactOwningTeamPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"search": &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
			"limit":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, ok := accesscontrol.ActorFromContext(p.Context)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			repository, err := artifactAccessRepository(s)
			if err != nil {
				return nil, err
			}
			limit, offset := bucketPageArgs(p)
			page, err := repository.ListArtifactOwningTeams(p.Context, store.ActorTeamSelectorQuery{
				ActorSubjectID: actor.SubjectID, Search: strings.TrimSpace(graphQLArgString(p, "search")), Limit: limit, Offset: offset,
			})
			if err != nil {
				return nil, errors.New("artifact owning teams unavailable")
			}
			return projectArtifactOwningTeamPage(page), nil
		},
	}
}

func artifactAccessRepository(s store.Store) (store.ArtifactAccessRepository, error) {
	repository, ok := s.(store.ArtifactAccessRepository)
	if !ok {
		return nil, errors.New("artifact access is unavailable")
	}
	return repository, nil
}

func projectArtifactBuildSelectorPage(page store.ArtifactSelectorPage) map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, map[string]interface{}{
			"resource_type": item.Resource.Type, "resource_id": item.Resource.ID.String(), "display_name": item.DisplayName,
		})
	}
	return map[string]interface{}{"items": items, "total": page.Total}
}

func projectArtifactOwningTeamPage(page store.ArtifactOwningTeamPage) map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, map[string]interface{}{"id": item.ID.String(), "name": item.Name, "slug": item.Slug})
	}
	return map[string]interface{}{"items": items, "total": page.Total}
}
