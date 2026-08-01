package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

func resolveArtifactPlanOwnerTeam(ctx context.Context, configStore store.ConfigRepository, configKey string, current *store.ConfigState, requested *uuid.UUID) (*uuid.UUID, error) {
	if current == nil {
		if requested == nil || *requested == uuid.Nil {
			return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "owner_team_id is required for a new artifact"}
		}
		owner := *requested
		return &owner, nil
	}
	owner, err := configStore.ResolveArtifactOwnerTeam(ctx, configKey)
	if err != nil || owner == uuid.Nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner is unavailable"}
	}
	// Ownership is immutable after the first apply. An omitted value means
	// "keep the existing owner"; a supplied mismatch is never interpreted as a transfer.
	if requested != nil && *requested != owner {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner team is immutable"}
	}
	return &owner, nil
}

func preflightArtifactOwnership(ctx context.Context, s store.Store, actor accesscontrol.Actor, ownerTeamID uuid.UUID, existingArtifactID *uuid.UUID, rawRequirements []byte) error {
	requirements, err := accesscontrol.UnmarshalRequiredPermissions(rawRequirements)
	if err != nil || len(requirements) == 0 {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "artifact permission snapshot is unavailable"}
	}
	repository, ok := s.(store.ArtifactAccessRepository)
	if !ok {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "artifact authorization is unavailable"}
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.artifact.ownership_preflight")
	defer span.End()
	decision, err := repository.PreflightArtifactOwnership(ctx, store.ArtifactOwnershipPreflight{
		ActorSubjectID: actor.SubjectID, OwnerTeamID: ownerTeamID, ExistingArtifactID: existingArtifactID, Requirements: requirements,
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

func preflightStoredArtifactPlan(ctx context.Context, s store.Store, actor accesscontrol.Actor, plan *store.ConfigPlan, current *store.ConfigState) error {
	if plan == nil || plan.OwnerTeamID == nil || *plan.OwnerTeamID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact owner is unavailable"}
	}
	return preflightArtifactOwnership(ctx, s, actor, *plan.OwnerTeamID, existingArtifactID(current), plan.RequiredPermissions)
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
