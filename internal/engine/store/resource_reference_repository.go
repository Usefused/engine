package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrResourceReferenceNotFound  = errors.New("resource reference not found")
	ErrResourceReferenceAmbiguous = errors.New("resource reference is ambiguous")
)

type ResourceReferenceKind string

const (
	ReferenceTeam       ResourceReferenceKind = "team"
	ReferenceUser       ResourceReferenceKind = "user"
	ReferenceService    ResourceReferenceKind = "service"
	ReferenceBucket     ResourceReferenceKind = "bucket"
	ReferenceApp        ResourceReferenceKind = "app"
	ReferenceAppFamily  ResourceReferenceKind = "app_family"
	ReferenceCredential ResourceReferenceKind = "credential"
)

type ResourceReferenceQuery struct {
	Kind       ResourceReferenceKind
	Value      string
	AppKind    string
	AppVersion string
	ParentID   uuid.UUID
	AllowedAll bool
	AllowedIDs []uuid.UUID
}

type ResourceReferenceResolver interface {
	ResolveResourceReference(context.Context, ResourceReferenceQuery) (uuid.UUID, error)
}

func validateResourceReferenceQuery(query ResourceReferenceQuery) error {
	if strings.TrimSpace(query.Value) == "" {
		return fmt.Errorf("%w: reference is required", ErrResourceReferenceNotFound)
	}
	switch query.Kind {
	case ReferenceTeam, ReferenceUser, ReferenceService, ReferenceBucket:
		return nil
	case ReferenceApp, ReferenceAppFamily:
		// SDK and MCP families share a permission repository, but callers may bind
		// a human name to one product namespace to prevent cross-kind resolution.
		if query.AppKind == "" || query.AppKind == "sdk" || query.AppKind == "mcp" {
			return nil
		}
	case ReferenceCredential:
		if query.ParentID != uuid.Nil {
			return nil
		}
	}
	return fmt.Errorf("%w: unsupported reference kind %q", ErrResourceReferenceNotFound, query.Kind)
}
