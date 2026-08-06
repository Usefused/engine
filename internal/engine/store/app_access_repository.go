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
	ErrAppOwnershipDenied      = errors.New("app ownership authorization denied")
	ErrAppOwnerMismatch        = errors.New("app owner is immutable")
	ErrInvalidAppAccessRequest = errors.New("invalid app access request")
)

type AppOwnershipPreflight struct {
	ActorSubjectID uuid.UUID
	OwnerTeamID    uuid.UUID
	// ExistingAppID is nil for creates. Updates may additionally be
	// authorized by an explicit app-manager team share on this app family.
	ExistingAppID *uuid.UUID
	// Requirements are produced by Engine plan resolution. HTTP/GraphQL input
	// must never be decoded into this field.
	Requirements []accesscontrol.Requirement
}

type AppOwnershipDecision struct {
	Allowed           bool
	MembershipAllowed bool
	ActorMissing      []accesscontrol.Requirement
	TeamMissing       []accesscontrol.Requirement
}

type AppSelectorQuery struct {
	ActorSubjectID uuid.UUID
	OwnerTeamID    uuid.UUID
	ResourceType   accesscontrol.ResourceType
	Search         string
	Limit          int
	Offset         int
}

type AppBuildSelector struct {
	Resource    accesscontrol.ResourceRef
	DisplayName string
}

type AppSelectorPage struct {
	Items []AppBuildSelector
	Total int
}

type ActorTeamSelectorQuery struct {
	ActorSubjectID uuid.UUID
	Search         string
	Limit          int
	Offset         int
}

type AppOwningTeamReferenceQuery struct {
	ActorSubjectID uuid.UUID
	Reference      string
}

type AppOwningTeam struct {
	ID   uuid.UUID
	Name string
	Slug string
}

type AppOwningTeamPage struct {
	Items []AppOwningTeam
	Total int
}

type AppAccessRepository interface {
	PreflightAppOwnership(context.Context, AppOwnershipPreflight) (AppOwnershipDecision, error)
	ListAppBuildSelectors(context.Context, AppSelectorQuery) (AppSelectorPage, error)
	ListAppOwningTeams(context.Context, ActorTeamSelectorQuery) (AppOwningTeamPage, error)
	ResolveAppOwningTeamReference(context.Context, AppOwningTeamReferenceQuery) (uuid.UUID, error)
}

type AppFamilyAccessResolver interface {
	ResolveAppFamilyAccess(context.Context, uuid.UUID, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

func validateOwnershipPreflight(input AppOwnershipPreflight) error {
	if input.ActorSubjectID == uuid.Nil || input.OwnerTeamID == uuid.Nil || len(input.Requirements) == 0 {
		return ErrInvalidAppAccessRequest
	}
	if input.ExistingAppID != nil && *input.ExistingAppID == uuid.Nil {
		return ErrInvalidAppAccessRequest
	}
	for _, requirement := range input.Requirements {
		if err := accesscontrol.ValidatePermission(requirement.Permission); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAppAccessRequest, err)
		}
		if err := accesscontrol.ValidateResourceType(requirement.Resource.Type); err != nil || requirement.Resource.ID == uuid.Nil {
			return ErrInvalidAppAccessRequest
		}
	}
	return nil
}

func validateSelectorQuery(input AppSelectorQuery) error {
	if input.ActorSubjectID == uuid.Nil || input.Limit < 1 || input.Limit > 200 || input.Offset < 0 {
		return ErrInvalidAppAccessRequest
	}
	if input.ResourceType != accesscontrol.ResourceService && input.ResourceType != accesscontrol.ResourceBucket {
		return ErrInvalidAppAccessRequest
	}
	if strings.TrimSpace(input.Search) != input.Search || len(input.Search) > 100 {
		return ErrInvalidAppAccessRequest
	}
	return nil
}

func validateOwningTeamQuery(input ActorTeamSelectorQuery) error {
	if input.ActorSubjectID == uuid.Nil || input.Limit < 1 || input.Limit > 200 || input.Offset < 0 {
		return ErrInvalidAppAccessRequest
	}
	if strings.TrimSpace(input.Search) != input.Search || len(input.Search) > 100 {
		return ErrInvalidAppAccessRequest
	}
	return nil
}

func validateOwningTeamReferenceQuery(input AppOwningTeamReferenceQuery) error {
	if input.ActorSubjectID == uuid.Nil || strings.TrimSpace(input.Reference) != input.Reference || input.Reference == "" || len(input.Reference) > 100 {
		return ErrInvalidAppAccessRequest
	}
	return nil
}
