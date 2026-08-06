package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var ErrInvalidWorkspaceShare = errors.New("invalid workspace share")

type WorkspaceShare struct {
	ID                  uuid.UUID
	RoleSlug            string
	RoleDisplayName     string
	Resource            accesscontrol.ResourceRef
	ResourceDisplayName string
	CreatedAt           time.Time
}

type WorkspaceShareMutation struct {
	Resource accesscontrol.ResourceRef
	Actor    MutationActor
}

type WorkspaceShareListOptions struct {
	ResourceType *accesscontrol.ResourceType
	Limit        int
	Offset       int
}

type WorkspaceShareMutationResult struct {
	Share                 WorkspaceShare
	AuthorizationRevision int64
	Changed               bool
}

type WorkspaceAccessRepository interface {
	ListWorkspaceShares(context.Context, WorkspaceShareListOptions) ([]WorkspaceShare, int, error)
	GrantWorkspaceShare(context.Context, WorkspaceShareMutation) (WorkspaceShareMutationResult, error)
	RevokeWorkspaceShare(context.Context, WorkspaceShareMutation) (WorkspaceShareMutationResult, error)
}

func validateWorkspaceShareMutation(input WorkspaceShareMutation) error {
	if input.Resource.ID == uuid.Nil || validateMutationActor(input.Actor) != nil {
		return ErrInvalidWorkspaceShare
	}
	if _, ok := workspaceShareRole(input.Resource.Type); !ok {
		return fmt.Errorf("%w: unsupported resource type %q", ErrInvalidWorkspaceShare, input.Resource.Type)
	}
	return nil
}

func validateWorkspaceShareListOptions(options WorkspaceShareListOptions) error {
	if options.Limit < 1 || options.Limit > 100 || options.Offset < 0 {
		return ErrInvalidWorkspaceShare
	}
	if options.ResourceType == nil {
		return nil
	}
	if _, ok := workspaceShareRole(*options.ResourceType); !ok {
		return ErrInvalidWorkspaceShare
	}
	return nil
}

func workspaceShareRole(resourceType accesscontrol.ResourceType) (string, bool) {
	// Workspace-wide access must never imply secret or app management;
	// owners keep those duties while every workspace member gets bounded use.
	switch resourceType {
	case accesscontrol.ResourceBucket:
		return accesscontrol.RoleBucketUser, true
	case accesscontrol.ResourceApp:
		return accesscontrol.RoleAppUser, true
	default:
		return "", false
	}
}
