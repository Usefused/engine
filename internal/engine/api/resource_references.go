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

const (
	graphQLCodeResourceNotFound  = "FUSED_RESOURCE_NOT_FOUND"
	graphQLCodeResourceAmbiguous = "FUSED_RESOURCE_AMBIGUOUS"
)

type safeGraphQLClientError struct {
	message string
	code    string
}

func (e safeGraphQLClientError) Error() string {
	return e.message
}

func (e safeGraphQLClientError) Extensions() map[string]interface{} {
	// Only fixed server-authored categories cross this boundary. The CLI can
	// improve its guidance without trusting a resolver message that might echo
	// user input or credentials.
	return map[string]interface{}{"code": e.code}
}

func requiredGraphQLResourceReference(p graphql.ResolveParams, source any, argument string, kind store.ResourceReferenceKind, parentID uuid.UUID) (uuid.UUID, error) {
	value := strings.TrimSpace(fmt.Sprint(p.Args[argument]))
	if value == "" {
		return uuid.Nil, fmt.Errorf("%s is required", argument)
	}
	// UUIDs remain an exact automation escape hatch. Mutating repositories
	// still verify the target, while human references take the indexed resolver.
	if id, err := uuid.Parse(value); err == nil {
		return id, nil
	}
	resolver, ok := source.(store.ResourceReferenceResolver)
	if !ok {
		return uuid.Nil, errors.New("resource reference resolution is unavailable")
	}
	id, err := resolver.ResolveResourceReference(p.Context, store.ResourceReferenceQuery{Kind: kind, Value: value, ParentID: parentID, AllowedAll: true})
	if err != nil {
		return uuid.Nil, resourceReferenceGraphQLError(err)
	}
	return id, nil
}

func referenceKindForResource(resourceType accesscontrol.ResourceType) (store.ResourceReferenceKind, error) {
	switch resourceType {
	case accesscontrol.ResourceService:
		return store.ReferenceService, nil
	case accesscontrol.ResourceBucket:
		return store.ReferenceBucket, nil
	case accesscontrol.ResourceArtifact:
		return store.ReferenceArtifact, nil
	default:
		return "", fmt.Errorf("resource type %q does not support a human reference", resourceType)
	}
}

func resourceReferenceGraphQLError(err error) error {
	if errors.Is(err, store.ErrResourceReferenceNotFound) {
		return safeGraphQLClientError{message: "resource was not found; use its name, slug, email, or full UUID", code: graphQLCodeResourceNotFound}
	}
	if errors.Is(err, store.ErrResourceReferenceAmbiguous) {
		return safeGraphQLClientError{message: "name exists as both an SDK and MCP server; use the full UUID", code: graphQLCodeResourceAmbiguous}
	}
	return err
}
