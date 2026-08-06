package api

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var appSelectorResourceTypeGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "AppSelectorResourceType",
	Values: graphql.EnumValueConfigMap{
		"SERVICE": &graphql.EnumValueConfig{Value: accesscontrol.ResourceService},
		"BUCKET":  &graphql.EnumValueConfig{Value: accesscontrol.ResourceBucket},
	},
})

var appBuildSelectorGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppBuildSelector",
	Fields: graphql.Fields{
		"resource_type": &graphql.Field{Type: graphql.NewNonNull(appSelectorResourceTypeGraphQLEnum)},
		"resource_id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"display_name":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var appBuildSelectorPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppBuildSelectorPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appBuildSelectorGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var appOwningTeamGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppOwningTeam",
	Fields: graphql.Fields{
		"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"slug": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var appOwningTeamPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppOwningTeamPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(appOwningTeamGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

func appBuildSelectorsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: appBuildSelectorPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"owner_team_id": &graphql.ArgumentConfig{Type: graphql.ID},
			"resource_type": &graphql.ArgumentConfig{Type: graphql.NewNonNull(appSelectorResourceTypeGraphQLEnum)},
			"search":        &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
			"limit":         &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset":        &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app_build_selectors")
			defer span.End()
			actor, ok := accesscontrol.ActorFromContext(ctx)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			repository, err := appAccessRepository(s)
			if err != nil {
				return nil, err
			}
			resolvedOwnerTeamID := uuid.Nil
			ownerTeamReference := strings.TrimSpace(graphQLArgString(p, "owner_team_id"))
			if ownerTeamReference != "" {
				// Team-facing commands accept a stable slug as well as a UUID. Resolve
				// it inside the actor-scoped repository so ineligible and unknown teams
				// are indistinguishable and clients never list/filter teams themselves.
				resolvedOwnerTeamID, err = repository.ResolveAppOwningTeamReference(ctx, store.AppOwningTeamReferenceQuery{
					ActorSubjectID: actor.SubjectID, Reference: ownerTeamReference,
				})
				if err != nil {
					return nil, resourceReferenceGraphQLError(err)
				}
			}
			resourceType, ok := p.Args["resource_type"].(accesscontrol.ResourceType)
			if !ok {
				return nil, errors.New("invalid resource_type")
			}
			limit, offset := bucketPageArgs(p)
			page, err := repository.ListAppBuildSelectors(ctx, store.AppSelectorQuery{
				ActorSubjectID: actor.SubjectID, OwnerTeamID: resolvedOwnerTeamID, ResourceType: resourceType,
				Search: strings.TrimSpace(graphQLArgString(p, "search")), Limit: limit, Offset: offset,
			})
			if err != nil {
				return nil, errors.New("app build selectors unavailable")
			}
			span.SetAttributes(attribute.Int("selector_count", len(page.Items)), attribute.String("resource_type", string(resourceType)))
			return projectAppBuildSelectorPage(page), nil
		},
	}
}

func appOwningTeamsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: appOwningTeamPageGraphQLType,
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
			repository, err := appAccessRepository(s)
			if err != nil {
				return nil, err
			}
			limit, offset := bucketPageArgs(p)
			page, err := repository.ListAppOwningTeams(p.Context, store.ActorTeamSelectorQuery{
				ActorSubjectID: actor.SubjectID, Search: strings.TrimSpace(graphQLArgString(p, "search")), Limit: limit, Offset: offset,
			})
			if err != nil {
				return nil, errors.New("app owning teams unavailable")
			}
			return projectAppOwningTeamPage(page), nil
		},
	}
}

func appAccessRepository(s store.Store) (store.AppAccessRepository, error) {
	repository, ok := s.(store.AppAccessRepository)
	if !ok {
		return nil, errors.New("app access is unavailable")
	}
	return repository, nil
}

func projectAppBuildSelectorPage(page store.AppSelectorPage) map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, map[string]interface{}{
			"resource_type": item.Resource.Type, "resource_id": item.Resource.ID.String(), "display_name": item.DisplayName,
		})
	}
	return map[string]interface{}{"items": items, "total": page.Total}
}

func projectAppOwningTeamPage(page store.AppOwningTeamPage) map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, map[string]interface{}{"id": item.ID.String(), "name": item.Name, "slug": item.Slug})
	}
	return map[string]interface{}{"items": items, "total": page.Total}
}
