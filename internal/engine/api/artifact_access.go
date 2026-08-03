package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type artifactOwner struct {
	subjectID *uuid.UUID
	teamID    *uuid.UUID
	teamSlug  string
}

type teamSlugResolver interface {
	GetTeamBySlug(context.Context, string) (store.Team, error)
}

func resolveArtifactPlanOwner(ctx context.Context, s store.Store, current *store.ConfigState, actor accesscontrol.Actor, requestedTeamSlug string) (artifactOwner, error) {
	requestedTeamSlug = strings.TrimSpace(requestedTeamSlug)
	if requestedTeamSlug != "" {
		teamRepository, ok := s.(teamSlugResolver)
		if !ok {
			return artifactOwner{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "team ownership is unavailable"}
		}
		team, err := teamRepository.GetTeamBySlug(ctx, requestedTeamSlug)
		if err != nil || team.Status != store.TeamStatusActive {
			return artifactOwner{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "owner team was not found or is archived"}
		}
		owner := artifactOwner{teamID: uuidPointer(team.ID), teamSlug: team.Slug}
		if current != nil && !ownerMatchesState(owner, current) {
			return artifactOwner{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner is immutable"}
		}
		return owner, nil
	}
	if current != nil {
		return artifactOwnerFromState(current)
	}
	if actor.SubjectID == uuid.Nil {
		return artifactOwner{}, accesscontrol.ErrAuthenticationRequired
	}
	// New artifacts belong to the authenticated subject unless the caller
	// explicitly selects a team by slug.
	return artifactOwner{subjectID: uuidPointer(actor.SubjectID)}, nil
}

func artifactOwnerFromState(state *store.ConfigState) (artifactOwner, error) {
	if state == nil || (state.OwnerSubjectID == nil) == (state.OwnerTeamID == nil) {
		return artifactOwner{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner is unavailable"}
	}
	return artifactOwner{subjectID: state.OwnerSubjectID, teamID: state.OwnerTeamID}, nil
}

func ownerMatchesState(owner artifactOwner, state *store.ConfigState) bool {
	if state == nil {
		return true
	}
	return equalOptionalUUID(owner.subjectID, state.OwnerSubjectID) && equalOptionalUUID(owner.teamID, state.OwnerTeamID)
}

func equalOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	copy := value
	return &copy
}

func preflightArtifactOwnership(ctx context.Context, s store.Store, actor accesscontrol.Actor, owner artifactOwner, existingArtifactID *uuid.UUID, rawRequirements []byte) error {
	requirements, err := accesscontrol.UnmarshalRequiredPermissions(rawRequirements)
	if err != nil || len(requirements) == 0 {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "artifact permission snapshot is unavailable"}
	}
	if owner.subjectID != nil {
		return preflightSubjectArtifactOwnership(ctx, actor, *owner.subjectID, existingArtifactID, requirements)
	}
	if owner.teamID == nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner is unavailable"}
	}
	repository, ok := s.(store.ArtifactAccessRepository)
	if !ok {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "artifact authorization is unavailable"}
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.artifact.ownership_preflight")
	defer span.End()
	decision, err := repository.PreflightArtifactOwnership(ctx, store.ArtifactOwnershipPreflight{
		ActorSubjectID: actor.SubjectID, OwnerTeamID: *owner.teamID, ExistingArtifactID: existingArtifactID, Requirements: requirements,
	})
	if err != nil {
		span.RecordError(err)
		return workspaceConfigHTTPError{status: http.StatusForbidden, message: "artifact owner authorization denied"}
	}
	span.SetAttributes(
		attribute.Bool("engine.authorization.allowed", decision.Allowed),
		attribute.Int("engine.authorization.actor_missing", len(decision.ActorMissing)),
		attribute.Int("engine.authorization.team_missing", len(decision.TeamMissing)),
	)
	if decision.Allowed {
		return nil
	}
	missing := append([]accesscontrol.Requirement(nil), decision.ActorMissing...)
	missing = append(missing, decision.TeamMissing...)
	if !decision.MembershipAllowed {
		missing = append(missing, accesscontrol.Requirement{
			Permission: accesscontrol.PermissionAccessManage,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
		})
	}
	if len(missing) == 0 {
		return accesscontrol.ErrPolicyDenied
	}
	return &accesscontrol.PermissionDeniedError{Missing: deduplicateArtifactRequirements(missing)}
}

func preflightSubjectArtifactOwnership(ctx context.Context, actor accesscontrol.Actor, ownerSubjectID uuid.UUID, existingArtifactID *uuid.UUID, requirements []accesscontrol.Requirement) error {
	authorizer := accesscontrol.SnapshotAuthorizer{}
	if err := authorizer.CheckAll(ctx, actor, requirements...); err != nil {
		return err
	}
	if actor.SubjectID == ownerSubjectID {
		return nil
	}
	if existingArtifactID == nil {
		// A pending personal create must not become a bearer capability: knowing its
		// plan receipt cannot let another Builder create a resource for that person.
		return accesscontrol.ErrPolicyDenied
	}
	// Explicit artifact managers may maintain a personally owned resource without
	// changing its immutable owner. The outer route enforces the same boundary;
	// repeating it here keeps direct handler use and future transports fail-closed.
	return authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{
		Permission: accesscontrol.PermissionArtifactManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *existingArtifactID},
	})
}

func preflightStoredArtifactPlan(ctx context.Context, s store.Store, actor accesscontrol.Actor, plan *store.ConfigPlan, current *store.ConfigState) error {
	if plan == nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner is unavailable"}
	}
	owner := artifactOwner{subjectID: plan.OwnerSubjectID, teamID: plan.OwnerTeamID}
	return preflightArtifactOwnership(ctx, s, actor, owner, existingArtifactID(current), plan.RequiredPermissions)
}

func existingArtifactID(current *store.ConfigState) *uuid.UUID {
	if current == nil || current.LatestResourceID == nil || *current.LatestResourceID == uuid.Nil {
		return nil
	}
	id := *current.LatestResourceID
	return &id
}

func loadAuthorizedArtifactPlanForApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, expected store.ConfigType) (*store.ConfigPlan, error) {
	plan, current, err := loadArtifactPlanForApply(ctx, configStore, call, expected)
	if err != nil {
		return nil, err
	}
	if err := preflightStoredArtifactPlan(ctx, s, call.actor, plan, current); err != nil {
		return nil, err
	}
	return plan, nil
}

func loadAuthorizedSDKPlanForApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall) (*store.ConfigPlan, error) {
	plan, current, err := loadSDKPlanForApply(ctx, configStore, call)
	if err != nil {
		return nil, err
	}
	if err := preflightStoredArtifactPlan(ctx, s, call.actor, plan, current); err != nil {
		return nil, err
	}
	return plan, nil
}

func deduplicateArtifactRequirements(requirements []accesscontrol.Requirement) []accesscontrol.Requirement {
	seen := make(map[accesscontrol.Requirement]struct{}, len(requirements))
	result := make([]accesscontrol.Requirement, 0, len(requirements))
	for _, requirement := range requirements {
		if _, exists := seen[requirement]; exists {
			continue
		}
		seen[requirement] = struct{}{}
		result = append(result, requirement)
	}
	return result
}

func isArtifactAuthorizationError(err error) bool {
	var denied *accesscontrol.PermissionDeniedError
	return errors.As(err, &denied) || errors.Is(err, accesscontrol.ErrAuthenticationRequired) || errors.Is(err, accesscontrol.ErrPolicyDenied)
}
