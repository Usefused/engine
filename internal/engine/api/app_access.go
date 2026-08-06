package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type configOwner struct {
	subjectID *uuid.UUID
	teamID    *uuid.UUID
	teamSlug  string
}

type teamSlugResolver interface {
	GetTeamBySlug(context.Context, string) (store.Team, error)
}

func resolveConfigPlanOwner(ctx context.Context, s store.Store, current *store.ConfigState, actor accesscontrol.Actor, requestedTeamSlug string) (configOwner, error) {
	requestedTeamSlug = strings.TrimSpace(requestedTeamSlug)
	if requestedTeamSlug != "" {
		teamRepository, ok := s.(teamSlugResolver)
		if !ok {
			return configOwner{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "team ownership is unavailable"}
		}
		team, err := teamRepository.GetTeamBySlug(ctx, requestedTeamSlug)
		if err != nil || team.Status != store.TeamStatusActive {
			return configOwner{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "owner team was not found or is archived"}
		}
		owner := configOwner{teamID: uuidPointer(team.ID), teamSlug: team.Slug}
		if current != nil && !ownerMatchesState(owner, current) {
			return configOwner{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "app owner is immutable"}
		}
		return owner, nil
	}
	if current != nil {
		return configOwnerFromState(current)
	}
	if actor.SubjectID == uuid.Nil {
		return configOwner{}, accesscontrol.ErrAuthenticationRequired
	}
	// New apps belong to the authenticated subject unless the caller
	// explicitly selects a team by slug.
	return configOwner{subjectID: uuidPointer(actor.SubjectID)}, nil
}

func configOwnerFromState(state *store.ConfigState) (configOwner, error) {
	if state == nil || (state.OwnerSubjectID == nil) == (state.OwnerTeamID == nil) {
		return configOwner{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "app owner is unavailable"}
	}
	return configOwner{subjectID: state.OwnerSubjectID, teamID: state.OwnerTeamID}, nil
}

func ownerMatchesState(owner configOwner, state *store.ConfigState) bool {
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

func preflightConfigOwnership(ctx context.Context, s store.Store, actor accesscontrol.Actor, owner configOwner, existingConfigResourceID *uuid.UUID, rawRequirements []byte) error {
	requirements, err := accesscontrol.UnmarshalRequiredPermissions(rawRequirements)
	if err != nil || len(requirements) == 0 {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "app permission snapshot is unavailable"}
	}
	if owner.subjectID != nil {
		return preflightSubjectConfigOwnership(ctx, s, actor, *owner.subjectID, existingConfigResourceID, requirements)
	}
	if owner.teamID == nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "app owner is unavailable"}
	}
	repository, ok := s.(store.AppAccessRepository)
	if !ok {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "app authorization is unavailable"}
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.app.ownership_preflight")
	defer span.End()
	decision, err := repository.PreflightAppOwnership(ctx, store.AppOwnershipPreflight{
		ActorSubjectID: actor.SubjectID, OwnerTeamID: *owner.teamID, ExistingAppID: existingConfigResourceID, Requirements: requirements,
	})
	if err != nil {
		span.RecordError(err)
		return workspaceConfigHTTPError{status: http.StatusForbidden, message: "app owner authorization denied"}
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
	return &accesscontrol.PermissionDeniedError{Missing: deduplicateConfigRequirements(missing)}
}

func preflightSubjectConfigOwnership(ctx context.Context, s store.Store, actor accesscontrol.Actor, ownerSubjectID uuid.UUID, existingConfigResourceID *uuid.UUID, requirements []accesscontrol.Requirement) error {
	authorizer := accesscontrol.SnapshotAuthorizer{}
	if err := authorizer.CheckAll(ctx, actor, requirements...); err != nil {
		return err
	}
	if actor.SubjectID == ownerSubjectID {
		return nil
	}
	if existingConfigResourceID == nil {
		// A pending personal create must not become a bearer capability: knowing its
		// plan receipt cannot let another Builder create a resource for that person.
		return accesscontrol.ErrPolicyDenied
	}
	app, err := s.GetApp(ctx, *existingConfigResourceID)
	if err != nil || app == nil || app.AccountID != actor.AccountID {
		return accesscontrol.ErrPolicyDenied
	}
	// Explicit app managers may maintain every version in a personally owned
	// family without changing its immutable owner.
	// The outer route enforces the same boundary; repeating it here keeps direct
	// handler use and future transports fail-closed.
	return authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAppManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID},
	})
}

func preflightStoredConfigPlan(ctx context.Context, s store.Store, actor accesscontrol.Actor, plan *store.ConfigPlan, current *store.ConfigState) error {
	if plan == nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "app owner is unavailable"}
	}
	owner := configOwner{subjectID: plan.OwnerSubjectID, teamID: plan.OwnerTeamID}
	return preflightConfigOwnership(ctx, s, actor, owner, plannedAppID(plan, current), plan.RequiredPermissions)
}

func plannedAppID(plan *store.ConfigPlan, current *store.ConfigState) *uuid.UUID {
	if id := existingConfigResourceID(current); id != nil {
		return id
	}
	var payload struct {
		AppID uuid.UUID `json:"app_id"`
	}
	if plan == nil || json.Unmarshal(plan.ResolvedPayload, &payload) != nil || payload.AppID == uuid.Nil {
		return nil
	}
	// Restored definitions have no config state yet, so the plan payload is the
	// authorization snapshot that carries their stable Registry identity.
	return &payload.AppID
}

func existingConfigResourceID(current *store.ConfigState) *uuid.UUID {
	if current == nil || current.LatestResourceID == nil || *current.LatestResourceID == uuid.Nil {
		return nil
	}
	id := *current.LatestResourceID
	return &id
}

func loadAuthorizedConfigPlanForApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, expected store.ConfigType) (*store.ConfigPlan, error) {
	plan, current, err := loadConfigPlanForApply(ctx, configStore, call, expected)
	if err != nil {
		return nil, err
	}
	if err := preflightStoredConfigPlan(ctx, s, call.actor, plan, current); err != nil {
		return nil, err
	}
	return plan, nil
}

func loadAuthorizedSDKAppPlanForApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall) (*store.ConfigPlan, error) {
	plan, current, err := loadSDKPlanForApply(ctx, configStore, call)
	if err != nil {
		return nil, err
	}
	if err := preflightStoredConfigPlan(ctx, s, call.actor, plan, current); err != nil {
		return nil, err
	}
	return plan, nil
}

func deduplicateConfigRequirements(requirements []accesscontrol.Requirement) []accesscontrol.Requirement {
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

func isConfigAuthorizationError(err error) bool {
	var denied *accesscontrol.PermissionDeniedError
	return errors.As(err, &denied) || errors.Is(err, accesscontrol.ErrAuthenticationRequired) || errors.Is(err, accesscontrol.ErrPolicyDenied)
}
