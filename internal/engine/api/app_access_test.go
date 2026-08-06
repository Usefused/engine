package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

func TestConfigPlanEnvelopeRejectsLegacyOwnerTeamID(t *testing.T) {
	request := httptest.NewRequest("POST", "/webhook-config/plan", strings.NewReader(`{
		"source_hash":"sha256:test","config_key":"webhook:test",
		"owner_team_id":"00000000-0000-0000-0000-000000000001",
		"config":{"apiVersion":"fused/v1","kind":"webhook","name":"test","services":{}}
	}`))
	if _, _, err := decodeWebhookConfigPlanRequest(request); err == nil || !strings.Contains(err.Error(), "invalid request body") {
		t.Fatalf("legacy owner_team_id error = %v", err)
	}
}

func TestResolveConfigPlanOwnerDefaultsToActorAndKeepsImmutableOwner(t *testing.T) {
	ctx := context.Background()
	actor := controlTestOwnerActor(uuid.New())
	repository := &appAccessGraphQLStore{}

	resolved, err := resolveConfigPlanOwner(ctx, repository, nil, actor, "")
	if err != nil || resolved.subjectID == nil || *resolved.subjectID != actor.SubjectID || resolved.teamID != nil {
		t.Fatalf("default owner = %#v, %v", resolved, err)
	}
	ownerTeamID := uuid.New()
	current := &store.ConfigState{ConfigKey: "sdk:existing:1.0.0", ConfigType: store.ConfigTypeSDK, OwnerTeamID: &ownerTeamID}
	resolved, err = resolveConfigPlanOwner(ctx, repository, current, actor, "")
	if err != nil || resolved.teamID == nil || *resolved.teamID != ownerTeamID {
		t.Fatalf("inferred owner = %v, %v", resolved, err)
	}
	repository.teamBySlug = store.Team{ID: uuid.New(), Slug: "other", Status: store.TeamStatusActive}
	if _, err := resolveConfigPlanOwner(ctx, repository, current, actor, "other"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("owner mismatch error = %v", err)
	}
}

func TestResolveConfigPlanOwnerResolvesActiveTeamSlug(t *testing.T) {
	team := store.Team{ID: uuid.New(), Slug: "platform", Status: store.TeamStatusActive}
	repository := &appAccessGraphQLStore{teamBySlug: team}
	owner, err := resolveConfigPlanOwner(context.Background(), repository, nil, controlTestOwnerActor(uuid.New()), team.Slug)
	if err != nil || owner.teamID == nil || *owner.teamID != team.ID || owner.subjectID != nil {
		t.Fatalf("team owner = %#v, %v", owner, err)
	}
}

func TestPreflightAppOwnershipReturnsStructuredServerDerivedDenial(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	ownerTeamID, existingConfigResourceID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	serviceRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionServiceConsume,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID},
	}
	bucketRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionBucketUse,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID},
	}
	repository := &appAccessGraphQLStore{decision: store.AppOwnershipDecision{
		MembershipAllowed: true, ActorMissing: []accesscontrol.Requirement{serviceRequirement}, TeamMissing: []accesscontrol.Requirement{bucketRequirement},
	}}
	raw, err := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{serviceRequirement, bucketRequirement})
	if err != nil {
		t.Fatalf("marshal requirements: %v", err)
	}
	err = preflightConfigOwnership(context.Background(), repository, actor, configOwner{teamID: &ownerTeamID}, &existingConfigResourceID, raw)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 2 {
		t.Fatalf("preflight error = %#v", err)
	}
	if repository.preflight.ActorSubjectID != actor.SubjectID || repository.preflight.OwnerTeamID != ownerTeamID {
		t.Fatalf("server preflight input = %#v", repository.preflight)
	}
	if repository.preflight.ExistingAppID == nil || *repository.preflight.ExistingAppID != existingConfigResourceID {
		t.Fatalf("existing app input = %#v, want %s", repository.preflight.ExistingAppID, existingConfigResourceID)
	}
	if len(repository.preflight.Requirements) != 2 {
		t.Fatalf("server-derived requirements = %#v", repository.preflight.Requirements)
	}
}

func TestPreflightAppOwnershipHidesMembershipReasonBehindAccessManage(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	actor.Authorization, _ = accesscontrol.NewAuthorizationSnapshot(1)
	workspaceRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAppCreate,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	raw, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{workspaceRequirement})
	repository := &appAccessGraphQLStore{decision: store.AppOwnershipDecision{MembershipAllowed: false}}
	ownerTeamID := uuid.New()
	err := preflightConfigOwnership(context.Background(), repository, actor, configOwner{teamID: &ownerTeamID}, nil, raw)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 1 || denied.Missing[0].Permission != accesscontrol.PermissionAccessManage {
		t.Fatalf("membership denial = %#v", err)
	}
}

func TestPreflightSubjectConfigOwnershipBindsPendingCreateToOwner(t *testing.T) {
	ownerSubjectID := uuid.New()
	actor := controlTestOwnerActor(uuid.New())
	requirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAppCreate,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	raw, err := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{requirement})
	if err != nil {
		t.Fatalf("marshal requirements: %v", err)
	}

	if err := preflightConfigOwnership(context.Background(), &appAccessGraphQLStore{}, actor, configOwner{subjectID: &ownerSubjectID}, nil, raw); !errors.Is(err, accesscontrol.ErrPolicyDenied) {
		t.Fatalf("cross-subject create error = %v, want policy denial", err)
	}
	if err := preflightConfigOwnership(context.Background(), &appAccessGraphQLStore{}, actor, configOwner{subjectID: &actor.SubjectID}, nil, raw); err != nil {
		t.Fatalf("owner create preflight = %v", err)
	}
}

func TestPreflightSubjectConfigOwnershipAllowsExplicitManagerUpdate(t *testing.T) {
	ownerSubjectID, appID, familyID := uuid.New(), uuid.New(), uuid.New()
	actor := controlTestOwnerActor(uuid.New())
	requirements := []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionServiceConsume,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()},
	}, {
		Permission: accesscontrol.PermissionAppManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID},
	}}
	actor.Authorization, _ = accesscontrol.NewAuthorizationSnapshot(1,
		accesscontrol.Grant{Permission: requirements[0].Permission, Resource: requirements[0].Resource},
		accesscontrol.Grant{Permission: requirements[1].Permission, Resource: requirements[1].Resource},
	)
	raw, _ := accesscontrol.MarshalRequiredPermissions(requirements)

	repository := &appAccessGraphQLStore{apps: map[uuid.UUID]store.App{
		appID: {AppID: appID, AppFamilyID: familyID, AccountID: actor.AccountID},
	}}
	if err := preflightConfigOwnership(context.Background(), repository, actor, configOwner{subjectID: &ownerSubjectID}, &appID, raw); err != nil {
		t.Fatalf("manager update preflight = %v", err)
	}
}

func TestStoredConfigPlanPreflightThreadsCurrentAppID(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	ownerTeamID, appID := uuid.New(), uuid.New()
	requirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAppManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: appID},
	}
	raw, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{requirement})
	configStore := &mockConfigStore{state: &store.ConfigState{ConfigKey: "sdk:shared", LatestResourceID: &appID}}
	repository := &appAccessGraphQLStore{decision: store.AppOwnershipDecision{Allowed: true}}
	plan := &store.ConfigPlan{ConfigKey: "sdk:shared", OwnerTeamID: &ownerTeamID, RequiredPermissions: raw}

	if err := preflightStoredConfigPlan(context.Background(), repository, actor, plan, configStore.state); err != nil {
		t.Fatalf("stored preflight: %v", err)
	}
	if repository.preflight.ExistingAppID == nil || *repository.preflight.ExistingAppID != appID {
		t.Fatalf("existing app = %#v, want %s", repository.preflight.ExistingAppID, appID)
	}
}
