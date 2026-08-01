package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

func TestResolveArtifactPlanOwnerTeamRequiresCreateAndInfersUpdate(t *testing.T) {
	ctx := context.Background()
	ownerTeamID := uuid.New()
	configStore := &mockConfigStore{ownerTeamID: ownerTeamID}

	if _, err := resolveArtifactPlanOwnerTeam(ctx, configStore, "sdk:new:1.0.0", nil, nil); err == nil || !strings.Contains(err.Error(), "owner_team_id is required") {
		t.Fatalf("new artifact error = %v", err)
	}
	current := &store.ConfigState{ConfigKey: "sdk:existing:1.0.0", ConfigType: store.ConfigTypeSDK}
	resolved, err := resolveArtifactPlanOwnerTeam(ctx, configStore, current.ConfigKey, current, nil)
	if err != nil || resolved == nil || *resolved != ownerTeamID {
		t.Fatalf("inferred owner = %v, %v", resolved, err)
	}
	otherTeamID := uuid.New()
	if _, err := resolveArtifactPlanOwnerTeam(ctx, configStore, current.ConfigKey, current, &otherTeamID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("owner mismatch error = %v", err)
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
	err = preflightArtifactOwnership(context.Background(), repository, actor, ownerTeamID, &existingArtifactID, raw)
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
	err := preflightArtifactOwnership(context.Background(), repository, actor, uuid.New(), nil, raw)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 1 || denied.Missing[0].Permission != accesscontrol.PermissionAccessManage {
		t.Fatalf("membership denial = %#v", err)
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
