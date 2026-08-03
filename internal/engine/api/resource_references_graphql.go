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

func artifactReferenceGraphQLField(s store.Store) *graphql.Field {
	field := resourceReferenceGraphQLField(s, store.ReferenceArtifact)
	field.Args["kind"] = &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""}
	return field
}

func resourceReferenceGraphQLField(s store.Store, kind store.ResourceReferenceKind) *graphql.Field {
	return &graphql.Field{
		Type: resourceReferenceGraphQLType,
		Args: graphql.FieldConfigArgument{"reference": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			value := strings.TrimSpace(fmt.Sprint(p.Args["reference"]))
			id, err := resolveVisibleResourceReference(p, s, kind, value, referenceArtifactKind(p, kind))
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"id": id.String(), "kind": string(kind)}, nil
		},
	}
}

func resolveVisibleResourceReference(p graphql.ResolveParams, s store.Store, kind store.ResourceReferenceKind, value, artifactKind string) (uuid.UUID, error) {
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
		Kind: kind, Value: value, ArtifactKind: artifactKind, AllowedAll: authorized.All, AllowedIDs: authorized.IDs,
	})
	if err != nil {
		return uuid.Nil, resourceReferenceGraphQLError(err)
	}
	return id, nil
}

func referenceArtifactKind(p graphql.ResolveParams, kind store.ResourceReferenceKind) string {
	if kind != store.ReferenceArtifact {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprint(p.Args["kind"])))
}

func referenceVisibility(kind store.ResourceReferenceKind) (accesscontrol.Permission, accesscontrol.ResourceType) {
	switch kind {
	case store.ReferenceService:
		return accesscontrol.PermissionServiceRead, accesscontrol.ResourceService
	case store.ReferenceArtifact:
		return accesscontrol.PermissionArtifactRead, accesscontrol.ResourceArtifact
	default:
		return accesscontrol.PermissionBucketRead, accesscontrol.ResourceBucket
	}
}
