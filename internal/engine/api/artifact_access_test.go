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

func TestArtifactPlanEnvelopeRejectsLegacyOwnerTeamID(t *testing.T) {
	request := httptest.NewRequest("POST", "/webhook-config/plan", strings.NewReader(`{
		"source_hash":"sha256:test","config_key":"webhook:test",
		"owner_team_id":"00000000-0000-0000-0000-000000000001",
		"config":{"apiVersion":"fused/v1","kind":"webhook","name":"test","services":{}}
	}`))
	if _, _, err := decodeWebhookConfigPlanRequest(request); err == nil || !strings.Contains(err.Error(), "invalid request body") {
		t.Fatalf("legacy owner_team_id error = %v", err)
	}
}

func TestResolveArtifactPlanOwnerDefaultsToActorAndKeepsImmutableOwner(t *testing.T) {
	ctx := context.Background()
	actor := controlTestOwnerActor(uuid.New())
	repository := &artifactAccessGraphQLStore{}

	resolved, err := resolveArtifactPlanOwner(ctx, repository, nil, actor, "")
	if err != nil || resolved.subjectID == nil || *resolved.subjectID != actor.SubjectID || resolved.teamID != nil {
		t.Fatalf("default owner = %#v, %v", resolved, err)
	}
	ownerTeamID := uuid.New()
	current := &store.ConfigState{ConfigKey: "sdk:existing:1.0.0", ConfigType: store.ConfigTypeSDK, OwnerTeamID: &ownerTeamID}
	resolved, err = resolveArtifactPlanOwner(ctx, repository, current, actor, "")
	if err != nil || resolved.teamID == nil || *resolved.teamID != ownerTeamID {
		t.Fatalf("inferred owner = %v, %v", resolved, err)
	}
	repository.teamBySlug = store.Team{ID: uuid.New(), Slug: "other", Status: store.TeamStatusActive}
	if _, err := resolveArtifactPlanOwner(ctx, repository, current, actor, "other"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("owner mismatch error = %v", err)
	}
}

func TestResolveArtifactPlanOwnerResolvesActiveTeamSlug(t *testing.T) {
	team := store.Team{ID: uuid.New(), Slug: "platform", Status: store.TeamStatusActive}
	repository := &artifactAccessGraphQLStore{teamBySlug: team}
	owner, err := resolveArtifactPlanOwner(context.Background(), repository, nil, controlTestOwnerActor(uuid.New()), team.Slug)
	if err != nil || owner.teamID == nil || *owner.teamID != team.ID || owner.subjectID != nil {
		t.Fatalf("team owner = %#v, %v", owner, err)
	}
}

func TestPreflightArtifactOwnershipReturnsStructuredServerDerivedDenial(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	ownerTeamID, existingArtifactID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	serviceRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionServiceConsume,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID},
	}
	bucketRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionBucketUse,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID},
	}
	repository := &artifactAccessGraphQLStore{decision: store.ArtifactOwnershipDecision{
		MembershipAllowed: true, ActorMissing: []accesscontrol.Requirement{serviceRequirement}, TeamMissing: []accesscontrol.Requirement{bucketRequirement},
	}}
	raw, err := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{serviceRequirement, bucketRequirement})
	if err != nil {
		t.Fatalf("marshal requirements: %v", err)
	}
	err = preflightArtifactOwnership(context.Background(), repository, actor, artifactOwner{teamID: &ownerTeamID}, &existingArtifactID, raw)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 2 {
		t.Fatalf("preflight error = %#v", err)
	}
	if repository.preflight.ActorSubjectID != actor.SubjectID || repository.preflight.OwnerTeamID != ownerTeamID {
		t.Fatalf("server preflight input = %#v", repository.preflight)
	}
	if repository.preflight.ExistingArtifactID == nil || *repository.preflight.ExistingArtifactID != existingArtifactID {
		t.Fatalf("existing artifact input = %#v, want %s", repository.preflight.ExistingArtifactID, existingArtifactID)
	}
	if len(repository.preflight.Requirements) != 2 {
		t.Fatalf("server-derived requirements = %#v", repository.preflight.Requirements)
	}
}

func TestPreflightArtifactOwnershipHidesMembershipReasonBehindAccessManage(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	actor.Authorization, _ = accesscontrol.NewAuthorizationSnapshot(1)
	workspaceRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionArtifactCreate,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	raw, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{workspaceRequirement})
	repository := &artifactAccessGraphQLStore{decision: store.ArtifactOwnershipDecision{MembershipAllowed: false}}
	ownerTeamID := uuid.New()
	err := preflightArtifactOwnership(context.Background(), repository, actor, artifactOwner{teamID: &ownerTeamID}, nil, raw)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 1 || denied.Missing[0].Permission != accesscontrol.PermissionAccessManage {
		t.Fatalf("membership denial = %#v", err)
	}
}

func TestPreflightSubjectArtifactOwnershipBindsPendingCreateToOwner(t *testing.T) {
	ownerSubjectID := uuid.New()
	actor := controlTestOwnerActor(uuid.New())
	requirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionArtifactCreate,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	raw, err := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{requirement})
	if err != nil {
		t.Fatalf("marshal requirements: %v", err)
	}

	if err := preflightArtifactOwnership(context.Background(), &artifactAccessGraphQLStore{}, actor, artifactOwner{subjectID: &ownerSubjectID}, nil, raw); !errors.Is(err, accesscontrol.ErrPolicyDenied) {
		t.Fatalf("cross-subject create error = %v, want policy denial", err)
	}
	if err := preflightArtifactOwnership(context.Background(), &artifactAccessGraphQLStore{}, actor, artifactOwner{subjectID: &actor.SubjectID}, nil, raw); err != nil {
		t.Fatalf("owner create preflight = %v", err)
	}
}

func TestPreflightSubjectArtifactOwnershipAllowsExplicitManagerUpdate(t *testing.T) {
	ownerSubjectID, artifactID := uuid.New(), uuid.New()
	actor := controlTestOwnerActor(uuid.New())
	requirements := []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionServiceConsume,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()},
	}, {
		Permission: accesscontrol.PermissionArtifactManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: artifactID},
	}}
	actor.Authorization, _ = accesscontrol.NewAuthorizationSnapshot(1,
		accesscontrol.Grant{Permission: requirements[0].Permission, Resource: requirements[0].Resource},
		accesscontrol.Grant{Permission: requirements[1].Permission, Resource: requirements[1].Resource},
	)
	raw, _ := accesscontrol.MarshalRequiredPermissions(requirements)

	if err := preflightArtifactOwnership(context.Background(), &artifactAccessGraphQLStore{}, actor, artifactOwner{subjectID: &ownerSubjectID}, &artifactID, raw); err != nil {
		t.Fatalf("manager update preflight = %v", err)
	}
}

func TestStoredArtifactPlanPreflightThreadsCurrentArtifactID(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	ownerTeamID, artifactID := uuid.New(), uuid.New()
	requirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionArtifactManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: artifactID},
	}
	raw, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{requirement})
	configStore := &mockConfigStore{state: &store.ConfigState{ConfigKey: "sdk:shared", LatestResourceID: &artifactID}}
	repository := &artifactAccessGraphQLStore{decision: store.ArtifactOwnershipDecision{Allowed: true}}
	plan := &store.ConfigPlan{ConfigKey: "sdk:shared", OwnerTeamID: &ownerTeamID, RequiredPermissions: raw}

	if err := preflightStoredArtifactPlan(context.Background(), repository, actor, plan, configStore.state); err != nil {
		t.Fatalf("stored preflight: %v", err)
	}
	if repository.preflight.ExistingArtifactID == nil || *repository.preflight.ExistingArtifactID != artifactID {
		t.Fatalf("existing artifact = %#v, want %s", repository.preflight.ExistingArtifactID, artifactID)
	}
}
