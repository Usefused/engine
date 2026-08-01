package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var (
	ErrArtifactOwnershipDenied      = errors.New("artifact ownership authorization denied")
	ErrArtifactOwnerTeamMismatch    = errors.New("artifact owner team is immutable")
	ErrInvalidArtifactAccessRequest = errors.New("invalid artifact access request")
)

type ArtifactOwnershipPreflight struct {
	ActorSubjectID uuid.UUID
	OwnerTeamID    uuid.UUID
	// ExistingArtifactID is nil for creates. Updates may additionally be
	// authorized by an explicit artifact-manager team share on this artifact.
	ExistingArtifactID *uuid.UUID
	// Requirements are produced by Engine plan resolution. HTTP/GraphQL input
	// must never be decoded into this field.
	Requirements []accesscontrol.Requirement
}

type ArtifactOwnershipDecision struct {
	Allowed           bool
	MembershipAllowed bool
	ActorMissing      []accesscontrol.Requirement
	TeamMissing       []accesscontrol.Requirement
}

type ArtifactSelectorQuery struct {
	ActorSubjectID uuid.UUID
	OwnerTeamID    uuid.UUID
	ResourceType   accesscontrol.ResourceType
	Search         string
	Limit          int
	Offset         int
}

type ArtifactBuildSelector struct {
	Resource    accesscontrol.ResourceRef
	DisplayName string
}

type ArtifactSelectorPage struct {
	Items []ArtifactBuildSelector
	Total int
}

type ActorTeamSelectorQuery struct {
	ActorSubjectID uuid.UUID
	Search         string
	Limit          int
	Offset         int
}

type ArtifactOwningTeam struct {
	ID   uuid.UUID
	Name string
	Slug string
}

type ArtifactOwningTeamPage struct {
	Items []ArtifactOwningTeam
	Total int
}

type ArtifactAccessRepository interface {
	PreflightArtifactOwnership(context.Context, ArtifactOwnershipPreflight) (ArtifactOwnershipDecision, error)
	ListArtifactBuildSelectors(context.Context, ArtifactSelectorQuery) (ArtifactSelectorPage, error)
	ListArtifactOwningTeams(context.Context, ActorTeamSelectorQuery) (ArtifactOwningTeamPage, error)
}

func validateOwnershipPreflight(input ArtifactOwnershipPreflight) error {
	if input.ActorSubjectID == uuid.Nil || input.OwnerTeamID == uuid.Nil || len(input.Requirements) == 0 {
		return ErrInvalidArtifactAccessRequest
	}
	if input.ExistingArtifactID != nil && *input.ExistingArtifactID == uuid.Nil {
		return ErrInvalidArtifactAccessRequest
	}
	for _, requirement := range input.Requirements {
		if err := accesscontrol.ValidatePermission(requirement.Permission); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidArtifactAccessRequest, err)
		}
		if err := accesscontrol.ValidateResourceType(requirement.Resource.Type); err != nil || requirement.Resource.ID == uuid.Nil {
			return ErrInvalidArtifactAccessRequest
		}
	}
	return nil
}

func validateSelectorQuery(input ArtifactSelectorQuery) error {
	if input.ActorSubjectID == uuid.Nil || input.OwnerTeamID == uuid.Nil || input.Limit < 1 || input.Limit > 200 || input.Offset < 0 {
		return ErrInvalidArtifactAccessRequest
	}
	if input.ResourceType != accesscontrol.ResourceService && input.ResourceType != accesscontrol.ResourceBucket {
		return ErrInvalidArtifactAccessRequest
	}
	if strings.TrimSpace(input.Search) != input.Search || len(input.Search) > 100 {
		return ErrInvalidArtifactAccessRequest
	}
	return nil
}

func validateOwningTeamQuery(input ActorTeamSelectorQuery) error {
	if input.ActorSubjectID == uuid.Nil || input.Limit < 1 || input.Limit > 200 || input.Offset < 0 {
		return ErrInvalidArtifactAccessRequest
	}
	if strings.TrimSpace(input.Search) != input.Search || len(input.Search) > 100 {
		return ErrInvalidArtifactAccessRequest
	}
	return nil
}
