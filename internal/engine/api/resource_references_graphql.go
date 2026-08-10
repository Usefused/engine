package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var resourceReferenceGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ResolvedResourceReference",
	Fields: graphql.Fields{
		"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"kind": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

func bucketReferenceGraphQLField(s store.Store) *graphql.Field {
	return resourceReferenceGraphQLField(s, store.ReferenceBucket)
}

func serviceReferenceGraphQLField(s store.Store) *graphql.Field {
	return resourceReferenceGraphQLField(s, store.ReferenceService)
}

func appReferenceGraphQLField(s store.Store) *graphql.Field {
	field := resourceReferenceGraphQLField(s, store.ReferenceApp)
	field.Args["kind"] = &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""}
	field.Args["version"] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}
	return field
}

func appFamilyReferenceGraphQLField(s store.Store) *graphql.Field {
	field := resourceReferenceGraphQLField(s, store.ReferenceAppFamily)
	field.Args["kind"] = &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""}
	return field
}

func resourceReferenceGraphQLField(s store.Store, kind store.ResourceReferenceKind) *graphql.Field {
	return &graphql.Field{
		Type: resourceReferenceGraphQLType,
		Args: graphql.FieldConfigArgument{"reference": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			value := strings.TrimSpace(fmt.Sprint(p.Args["reference"]))
			id, err := resolveVisibleResourceReference(p, s, kind, value, referenceAppKind(p, kind), referenceAppVersion(p, kind))
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"id": id.String(), "kind": string(kind)}, nil
		},
	}
}

func resolveVisibleResourceReference(p graphql.ResolveParams, s store.Store, kind store.ResourceReferenceKind, value string, appKind store.AppKind, appVersion string) (uuid.UUID, error) {
	resolver, ok := s.(store.ResourceReferenceResolver)
	if !ok {
		return uuid.Nil, errors.New("resource reference resolution is unavailable")
	}
	permission, resourceType := referenceVisibility(kind)
	authorized, err := graphQLAuthorizedScope(p.Context, permission, resourceType)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := resolver.ResolveResourceReference(p.Context, store.ResourceReferenceQuery{
		Kind: kind, Value: value, AppKind: store.AppKind(appKind), AppVersion: appVersion, AllowedAll: authorized.All, AllowedIDs: authorized.IDs,
	})
	if err != nil {
		return uuid.Nil, resourceReferenceGraphQLError(err)
	}
	return id, nil
}

func referenceAppKind(p graphql.ResolveParams, kind store.ResourceReferenceKind) store.AppKind {
	if kind != store.ReferenceApp && kind != store.ReferenceAppFamily {
		return ""
	}
	return store.AppKind(strings.ToLower(strings.TrimSpace(fmt.Sprint(p.Args["kind"]))))
}

func referenceAppVersion(p graphql.ResolveParams, kind store.ResourceReferenceKind) string {
	if kind != store.ReferenceApp {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(p.Args["version"]))
}

func referenceVisibility(kind store.ResourceReferenceKind) (accesscontrol.Permission, accesscontrol.ResourceType) {
	switch kind {
	case store.ReferenceService:
		return accesscontrol.PermissionServiceRead, accesscontrol.ResourceService
	case store.ReferenceApp, store.ReferenceAppFamily:
		return accesscontrol.PermissionAppRead, accesscontrol.ResourceApp
	default:
		return accesscontrol.PermissionBucketRead, accesscontrol.ResourceBucket
	}
}
