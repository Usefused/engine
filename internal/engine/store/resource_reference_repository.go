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
	ReferenceArtifact   ResourceReferenceKind = "artifact"
	ReferenceCredential ResourceReferenceKind = "credential"
)

type ResourceReferenceQuery struct {
	Kind         ResourceReferenceKind
	Value        string
	ArtifactKind string
	ParentID     uuid.UUID
	AllowedAll   bool
	AllowedIDs   []uuid.UUID
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
	case ReferenceArtifact:
		// SDK and MCP share a permission repository, but callers may bind a human
		// name to one product namespace to prevent cross-kind resolution.
		if query.ArtifactKind == "" || query.ArtifactKind == "sdk" || query.ArtifactKind == "mcp" {
			return nil
		}
	case ReferenceCredential:
		if query.ParentID != uuid.Nil {
			return nil
		}
	}
	return fmt.Errorf("%w: unsupported reference kind %q", ErrResourceReferenceNotFound, query.Kind)
}

func parseArtifactReference(value string) (string, string) {
	name, version, found := strings.Cut(strings.TrimSpace(value), "@")
	if !found {
		return strings.TrimSpace(name), ""
	}
	return strings.TrimSpace(name), strings.TrimSpace(version)
}
