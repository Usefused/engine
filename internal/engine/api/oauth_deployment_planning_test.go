package api

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

type deploymentPlanningStore struct {
	store.Store
	app    *store.App
	family *store.AppFamily
}

// GetApp returns the approved immutable anchor used only for family authorization.
func (s deploymentPlanningStore) GetApp(context.Context, uuid.UUID) (*store.App, error) {
	return s.app, nil
}

// GetAppFamilyByIdentity supplies the authoritative family resolved from the new release's name.
func (s deploymentPlanningStore) GetAppFamilyByIdentity(context.Context, uuid.UUID, string, string) (*store.AppFamily, error) {
	return s.family, nil
}

// TestDeploymentNewVersionRequiresFamilyManageWithoutCreate keeps owner/permissions separate from version immutability state.
func TestDeploymentNewVersionRequiresFamilyManageWithoutCreate(t *testing.T) {
	account, workspace, subject, familyID, appID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repository := deploymentPlanningStore{app: &store.App{AppID: appID, AccountID: account, AppFamilyID: familyID}, family: &store.AppFamily{AppFamilyID: familyID, AccountID: account, Kind: store.AppKindSDK, CanonicalName: "support", DeliveryMode: store.AppDeliveryModeAPI, OwnerSubjectID: subject}}
	generate := false
	call := sdkPlanCall{accountID: account, authorizationAppID: appID, document: sdkConfigDocument{Kind: "sdk", Name: "support", Version: "2.0.0", Generate: &generate}}
	state, authorizationID, err := deploymentPlanAuthorization(t.Context(), repository, call, nil, uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, appID, authorizationID)
	require.Equal(t, subject, *state.OwnerSubjectID)
	// The new version's actual config state remains nil; the synthetic state is used only for management authority.
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, accesscontrol.Grant{Permission: accesscontrol.PermissionAppAPIManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}})
	require.NoError(t, err)
	actor := accesscontrol.Actor{AccountID: account, WorkspaceID: workspace, SubjectID: subject, Authorization: snapshot}
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	required, _, err := configPlanRequiredPermissionsWithBuckets(ctx, repository, state, nil, nil, "support", "api")
	require.NoError(t, err)
	require.Contains(t, string(required), "app.api.manage")
	require.NotContains(t, string(required), "app.api.create")
	owner, err := configOwnerFromState(state)
	require.NoError(t, err)
	require.NoError(t, preflightConfigOwnership(ctx, repository, actor, owner, &authorizationID, required))
	// Apply recovers the authorization anchor without treating it as the newly generated AppID.
	payload, err := json.Marshal(appResolvedPayload{AuthorizationAppID: authorizationID})
	require.NoError(t, err)
	require.Equal(t, &authorizationID, plannedAppID(&store.ConfigPlan{ResolvedPayload: payload}, nil))
	var decoded appResolvedPayload
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, uuid.Nil, decoded.AppID)
	// A reused name pointing at another family must fail before planning.
	repository.family.AppFamilyID = uuid.New()
	_, _, err = deploymentPlanAuthorization(ctx, repository, call, nil, uuid.Nil)
	require.Error(t, err)
}
