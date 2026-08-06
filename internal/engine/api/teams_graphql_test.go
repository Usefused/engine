package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type teamGraphQLTestStore struct {
	store.Store
	teams             []store.Team
	total             int
	listOptions       store.TeamListOptions
	listCalls         int
	getResult         store.Team
	getErr            error
	createResults     []store.TeamMutationResult
	createErr         error
	createCalls       int
	createSideEffects int
	lastCreate        store.TeamMutation
	updateResult      store.TeamMutationResult
	updateErr         error
	updateCalls       int
	lastPatch         store.TeamPatch
	archiveResult     store.TeamMutationResult
	archiveErr        error
	archiveCalls      int
	addResult         store.TeamBindingMutationResult
	addErr            error
	addCalls          int
	addSideEffects    int
	lastBinding       store.TeamBindingMutation
	removeResult      store.TeamBindingMutationResult
	removeErr         error
	removeCalls       int
	clearResult       store.TeamBindingMutationResult
	clearErr          error
	clearCalls        int
	clearTeamID       uuid.UUID
	clearWorkspaceID  uuid.UUID
	clearActor        store.MutationActor
	workspaceShares   []store.WorkspaceShare
	workspaceTotal    int
	workspaceOptions  store.WorkspaceShareListOptions
	workspaceGrant    store.WorkspaceShareMutationResult
	workspaceRevoke   store.WorkspaceShareMutationResult
	workspaceMutation store.WorkspaceShareMutation
	referenceIDs      map[string]uuid.UUID
	referenceQueries  []store.ResourceReferenceQuery
}

func (s *teamGraphQLTestStore) ResolveResourceReference(_ context.Context, query store.ResourceReferenceQuery) (uuid.UUID, error) {
	s.referenceQueries = append(s.referenceQueries, query)
	return s.referenceIDs[string(query.Kind)+":"+query.Value], nil
}

type teamRevisionSink struct {
	revisions []int64
}

func (s *teamRevisionSink) SetRevision(revision int64) bool {
	s.revisions = append(s.revisions, revision)
	return true
}

func (s *teamGraphQLTestStore) CreateTeam(_ context.Context, input store.TeamMutation) (store.TeamMutationResult, error) {
	s.createCalls++
	s.lastCreate = input
	if s.createErr != nil {
		return store.TeamMutationResult{}, s.createErr
	}
	index := s.createCalls - 1
	if index >= len(s.createResults) {
		index = len(s.createResults) - 1
	}
	if index < 0 {
		return store.TeamMutationResult{}, nil
	}
	if s.createResults[index].Changed {
		s.createSideEffects++
	}
	return s.createResults[index], nil
}

func (s *teamGraphQLTestStore) GetTeam(_ context.Context, _ uuid.UUID) (store.Team, error) {
	return s.getResult, s.getErr
}

func (s *teamGraphQLTestStore) GetTeamBySlug(_ context.Context, _ string) (store.Team, error) {
	return s.getResult, s.getErr
}

func (s *teamGraphQLTestStore) ListTeams(_ context.Context, options store.TeamListOptions) ([]store.Team, int, error) {
	s.listCalls++
	s.listOptions = options
	return s.teams, s.total, nil
}

func (s *teamGraphQLTestStore) ListAuthorizedWorkspaceServicesPage(context.Context, accesscontrol.AuthorizedScope, []string, int, int) ([]store.WorkspaceService, int, error) {
	return []store.WorkspaceService{}, 0, nil
}

func (s *teamGraphQLTestStore) ListWorkspaceServiceVersionsForServices(context.Context, []uuid.UUID) (map[uuid.UUID][]store.WorkspaceServiceVersion, error) {
	return map[uuid.UUID][]store.WorkspaceServiceVersion{}, nil
}

func (s *teamGraphQLTestStore) ListAuthorizedBucketSummaries(context.Context, accesscontrol.AuthorizedScope, int, int) ([]store.BucketSummary, int, error) {
	return []store.BucketSummary{}, 0, nil
}

func (s *teamGraphQLTestStore) ListWorkspaceShares(_ context.Context, options store.WorkspaceShareListOptions) ([]store.WorkspaceShare, int, error) {
	s.workspaceOptions = options
	return s.workspaceShares, s.workspaceTotal, nil
}

func (s *teamGraphQLTestStore) GrantWorkspaceShare(_ context.Context, input store.WorkspaceShareMutation) (store.WorkspaceShareMutationResult, error) {
	s.workspaceMutation = input
	return s.workspaceGrant, nil
}

func (s *teamGraphQLTestStore) RevokeWorkspaceShare(_ context.Context, input store.WorkspaceShareMutation) (store.WorkspaceShareMutationResult, error) {
	s.workspaceMutation = input
	return s.workspaceRevoke, nil
}

func (s *teamGraphQLTestStore) UpdateTeam(_ context.Context, _ uuid.UUID, patch store.TeamPatch) (store.TeamMutationResult, error) {
	s.updateCalls++
	s.lastPatch = patch
	return s.updateResult, s.updateErr
}

func (s *teamGraphQLTestStore) ArchiveTeam(_ context.Context, _ uuid.UUID, _ store.MutationActor) (store.TeamMutationResult, error) {
	s.archiveCalls++
	return s.archiveResult, s.archiveErr
}

func (s *teamGraphQLTestStore) AddTeamBinding(_ context.Context, input store.TeamBindingMutation) (store.TeamBindingMutationResult, error) {
	s.addCalls++
	s.lastBinding = input
	if s.addErr == nil && s.addResult.Changed {
		s.addSideEffects++
	}
	return s.addResult, s.addErr
}

func (s *teamGraphQLTestStore) RemoveTeamBinding(_ context.Context, input store.TeamBindingMutation) (store.TeamBindingMutationResult, error) {
	s.removeCalls++
	s.lastBinding = input
	return s.removeResult, s.removeErr
}

func (s *teamGraphQLTestStore) ClearTeamWorkspaceRole(_ context.Context, teamID, workspaceID uuid.UUID, actor store.MutationActor) (store.TeamBindingMutationResult, error) {
	s.clearCalls++
	s.clearTeamID = teamID
	s.clearWorkspaceID = workspaceID
	s.clearActor = actor
	return s.clearResult, s.clearErr
}

func TestTeamsGraphQLProjectsBatchedBindingsAndOptions(t *testing.T) {
	now := time.Now().UTC()
	teamID, bucketID := uuid.New(), uuid.New()
	s := &teamGraphQLTestStore{total: 1, teams: []store.Team{{
		ID: teamID, Name: "Platform", Slug: "platform", Description: "Shared platform", Status: store.TeamStatusActive,
		CreatedAt: now, UpdatedAt: now, Bindings: []store.TeamBinding{{
			ID: uuid.New(), TeamID: teamID, RoleSlug: accesscontrol.RoleBucketManager, RoleDisplayName: "Bucket manager",
			Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}, ResourceDisplayName: "Production", CreatedAt: now,
		}},
	}}}
	response := executeTeamGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), `query {
		teams(search:"plat", limit:5, offset:2, include_archived:true) {
			items { id name bindings { role_slug resource_id resource_display_name } } total
		}
	}`)

	if s.listCalls != 1 || len(s.listOptions.Statuses) != 2 || s.listOptions.Search != "plat" || s.listOptions.Limit != 5 || s.listOptions.Offset != 2 {
		t.Fatalf("list call/options = %d %#v", s.listCalls, s.listOptions)
	}
	page := responseData(t, response)["teams"].(map[string]interface{})
	item := page["items"].([]interface{})[0].(map[string]interface{})
	binding := item["bindings"].([]interface{})[0].(map[string]interface{})
	if binding["resource_display_name"] != "Production" || binding["resource_id"] != bucketID.String() {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestTeamGraphQLRoleAuthorizationMatrix(t *testing.T) {
	team := testGraphQLTeam()
	tests := []struct {
		role          string
		query         string
		wantForbidden bool
		wantCalls     int
	}{
		{role: accesscontrol.RoleOwner, query: `query { teams { total } }`, wantCalls: 1},
		{role: accesscontrol.RoleAdmin, query: `query { teams { total } }`, wantCalls: 1},
		{role: accesscontrol.RoleBuilder, query: `query { teams { total } }`, wantForbidden: true},
		{role: accesscontrol.RoleViewer, query: `query { teams { total } }`, wantForbidden: true},
		{role: accesscontrol.RoleOwner, query: `mutation { createTeam(input:{name:"Platform"}) { changed } }`, wantCalls: 1},
		{role: accesscontrol.RoleAdmin, query: `mutation { createTeam(input:{name:"Platform"}) { changed } }`, wantCalls: 1},
		{role: accesscontrol.RoleBuilder, query: `mutation { createTeam(input:{name:"Platform"}) { changed } }`, wantForbidden: true},
		{role: accesscontrol.RoleViewer, query: `mutation { createTeam(input:{name:"Platform"}) { changed } }`, wantForbidden: true},
	}
	for _, test := range tests {
		t.Run(test.role+test.query[:5], func(t *testing.T) {
			s := &teamGraphQLTestStore{teams: []store.Team{team}, total: 1, createResults: []store.TeamMutationResult{{Team: team, Changed: true}}}
			response := executeTeamGraphQL(t, s, actorForBuiltInRole(t, test.role), test.query)
			if test.wantForbidden {
				if response.Code != http.StatusForbidden || s.listCalls+s.createCalls != 0 {
					t.Fatalf("status/calls = %d/%d, want 403/0: %s", response.Code, s.listCalls+s.createCalls, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK || s.listCalls+s.createCalls != test.wantCalls {
				t.Fatalf("status/calls = %d/%d, want 200/%d: %s", response.Code, s.listCalls+s.createCalls, test.wantCalls, response.Body.String())
			}
		})
	}
}

func TestTeamGraphQLAllOperationsFailClosedWithoutRequiredPermission(t *testing.T) {
	teamID, resourceID := uuid.New(), uuid.New()
	tests := []struct {
		name  string
		actor accesscontrol.Actor
		query string
	}{
		{name: "teams", actor: actorWithTeamPermissions(t), query: `query { teams { total } }`},
		{name: "team", actor: actorWithTeamPermissions(t), query: `query { team(id:"` + teamID.String() + `") { id } }`},
		{name: "create", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { createTeam(input:{name:"A"}) { changed } }`},
		{name: "update", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { updateTeam(id:"` + teamID.String() + `",input:{name:"A"}) { changed } }`},
		{name: "archive", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { archiveTeam(id:"` + teamID.String() + `") { changed } }`},
		{name: "workspace role", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { setTeamWorkspaceRole(team_id:"` + teamID.String() + `",role:VIEWER) { changed } }`},
		{name: "clear workspace role", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { setTeamWorkspaceRole(team_id:"` + teamID.String() + `") { changed } }`},
		{name: "grant service", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { grantTeamServiceAccess(team_id:"` + teamID.String() + `",service_id:"` + resourceID.String() + `",level:USER) { changed } }`},
		{name: "revoke service", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { revokeTeamServiceAccess(team_id:"` + teamID.String() + `",service_id:"` + resourceID.String() + `",level:USER) { changed } }`},
		{name: "grant bucket", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { grantTeamBucketAccess(team_id:"` + teamID.String() + `",bucket_id:"` + resourceID.String() + `",level:USER) { changed } }`},
		{name: "revoke bucket", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { revokeTeamBucketAccess(team_id:"` + teamID.String() + `",bucket_id:"` + resourceID.String() + `",level:USER) { changed } }`},
		{name: "grant artifact", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { grantTeamAppAccess(team_id:"` + teamID.String() + `",app_family_id:"` + resourceID.String() + `",level:READER) { changed } }`},
		{name: "revoke artifact", actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query: `mutation { revokeTeamAppAccess(team_id:"` + teamID.String() + `",app_family_id:"` + resourceID.String() + `",level:MANAGER) { changed } }`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &teamGraphQLTestStore{}
			response := executeTeamGraphQL(t, s, test.actor, test.query)
			calls := s.listCalls + s.createCalls + s.updateCalls + s.archiveCalls + s.addCalls + s.removeCalls + s.clearCalls
			if response.Code != http.StatusForbidden || calls != 0 {
				t.Fatalf("status/calls = %d/%d, want 403/0: %s", response.Code, calls, response.Body.String())
			}
		})
	}
}

func TestCreateTeamGraphQLDerivesSlugAndPassesActor(t *testing.T) {
	team := testGraphQLTeam()
	s := &teamGraphQLTestStore{createResults: []store.TeamMutationResult{{Team: team, Changed: true}}}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	query := `mutation { createTeam(input:{name:"Platform Team", description:"Shared"}) { team { slug } authorization_revision changed } }`
	response := executeTeamGraphQL(t, s, actor, query)

	if s.createCalls != 1 || s.createSideEffects != 1 || s.lastCreate.Slug != "platform-team" || s.lastCreate.Actor.SubjectID != actor.SubjectID || s.lastCreate.Actor.CredentialID != actor.CredentialID {
		t.Fatalf("create state = calls:%d effects:%d input:%#v", s.createCalls, s.createSideEffects, s.lastCreate)
	}
	if !mutationChanged(t, response, "createTeam") {
		t.Fatalf("create response = %s", response.Body.String())
	}
}

func TestTeamGraphQLConflictAndArchivedErrorsHaveNoSideEffects(t *testing.T) {
	teamID, serviceID := uuid.New(), uuid.New()
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	tests := []struct {
		name      string
		store     *teamGraphQLTestStore
		query     string
		wantError string
		wantCalls int
	}{
		{name: "slug conflict", store: &teamGraphQLTestStore{createErr: store.ErrTeamSlugConflict}, query: `mutation { createTeam(input:{name:"Duplicate"}) { changed } }`, wantError: "team slug already exists", wantCalls: 1},
		{name: "archive conflict", store: &teamGraphQLTestStore{archiveErr: store.ErrTeamArchiveConflict}, query: `mutation { archiveTeam(id:"` + teamID.String() + `") { changed } }`, wantError: "team cannot be archived while it has active access", wantCalls: 1},
		{name: "archived binding", store: &teamGraphQLTestStore{addErr: store.ErrTeamArchived}, query: `mutation { grantTeamServiceAccess(team_id:"` + teamID.String() + `",service_id:"` + serviceID.String() + `",level:USER) { changed } }`, wantError: "archived team cannot be changed", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := executeTeamGraphQL(t, test.store, actor, test.query)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.wantError) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			calls := test.store.createCalls + test.store.archiveCalls + test.store.addCalls
			if calls != test.wantCalls || test.store.createSideEffects != 0 {
				t.Fatalf("calls/effects = %d/%d", calls, test.store.createSideEffects)
			}
		})
	}
}

func TestTeamAppBindingRejectsWebhookConfigStateIDWithoutRevisionChange(t *testing.T) {
	teamID, webhookStateID := uuid.New(), uuid.New()
	s := &teamGraphQLTestStore{addErr: store.ErrInvalidTeamBinding}
	sink := &teamRevisionSink{}
	query := `mutation { grantTeamAppAccess(team_id:"` + teamID.String() + `",app_family_id:"` + webhookStateID.String() + `",level:MANAGER) { changed authorization_revision } }`

	response := executeTeamGraphQLWithSink(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), query, sink)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "invalid team request") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if s.addCalls != 1 || s.addSideEffects != 0 || len(sink.revisions) != 0 {
		t.Fatalf("calls/effects/revisions = %d/%d/%#v", s.addCalls, s.addSideEffects, sink.revisions)
	}
	if strings.Contains(response.Body.String(), webhookStateID.String()) {
		t.Fatalf("safe rejection exposed webhook state identity: %s", response.Body.String())
	}
}

func TestTeamGraphQLErrorHidesArchiveConflictCounts(t *testing.T) {
	err := teamGraphQLError(&store.TeamArchiveConflictError{BindingCount: 17, ActiveAppCount: 23})
	if err.Error() != "team cannot be archived while it has active access" {
		t.Fatalf("archive conflict error = %q", err)
	}
}

func TestTeamBindingGraphQLUsesExactRolesAndWorkspaceScope(t *testing.T) {
	teamID, serviceID := uuid.New(), uuid.New()
	binding := store.TeamBinding{ID: uuid.New(), TeamID: teamID, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, CreatedAt: time.Now()}
	s := &teamGraphQLTestStore{addResult: store.TeamBindingMutationResult{Binding: binding, Changed: true}, removeResult: store.TeamBindingMutationResult{Binding: binding}}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)

	executeTeamGraphQL(t, s, actor, `mutation { setTeamWorkspaceRole(team_id:"`+teamID.String()+`",role:BUILDER) { changed } }`)
	if s.lastBinding.RoleSlug != accesscontrol.RoleBuilder || s.lastBinding.Resource.Type != accesscontrol.ResourceWorkspace || s.lastBinding.Resource.ID != actor.WorkspaceID {
		t.Fatalf("workspace binding = %#v", s.lastBinding)
	}
	executeTeamGraphQL(t, s, actor, `mutation { revokeTeamServiceAccess(team_id:"`+teamID.String()+`",service_id:"`+serviceID.String()+`",level:MANAGER) { changed } }`)
	if s.removeCalls != 1 || s.lastBinding.RoleSlug != accesscontrol.RoleServiceManager || s.lastBinding.Resource.ID != serviceID {
		t.Fatalf("service revoke = %#v calls=%d", s.lastBinding, s.removeCalls)
	}
	executeTeamGraphQL(t, s, actor, `mutation { grantTeamAppAccess(team_id:"`+teamID.String()+`",app_family_id:"`+serviceID.String()+`",level:READER) { changed } }`)
	if s.addCalls != 2 || s.lastBinding.RoleSlug != accesscontrol.RoleAppReader || s.lastBinding.Resource.Type != accesscontrol.ResourceApp || s.lastBinding.Resource.ID != serviceID {
		t.Fatalf("artifact reader grant = %#v calls=%d", s.lastBinding, s.addCalls)
	}
	executeTeamGraphQL(t, s, actor, `mutation { grantTeamAppAccess(team_id:"`+teamID.String()+`",app_family_id:"`+serviceID.String()+`",level:USER) { changed } }`)
	if s.addCalls != 3 || s.lastBinding.RoleSlug != accesscontrol.RoleAppUser {
		t.Fatalf("artifact user grant = %#v calls=%d", s.lastBinding, s.addCalls)
	}
	executeTeamGraphQL(t, s, actor, `mutation { revokeTeamAppAccess(team_id:"`+teamID.String()+`",app_family_id:"`+serviceID.String()+`",level:MANAGER) { changed } }`)
	if s.removeCalls != 2 || s.lastBinding.RoleSlug != accesscontrol.RoleAppManager || s.lastBinding.Resource.Type != accesscontrol.ResourceApp {
		t.Fatalf("artifact manager revoke = %#v calls=%d", s.lastBinding, s.removeCalls)
	}
}

func TestTeamAppBindingGraphQLInvalidatesRevisionOnlyAfterChangedMutation(t *testing.T) {
	teamID, appID := uuid.New(), uuid.New()
	binding := store.TeamBinding{ID: uuid.New(), TeamID: teamID, RoleSlug: accesscontrol.RoleAppManager, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: appID}, CreatedAt: time.Now()}
	s := &teamGraphQLTestStore{addResult: store.TeamBindingMutationResult{Binding: binding, AuthorizationRevision: 44, Changed: true}}
	sink := &teamRevisionSink{}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	query := `mutation { grantTeamAppAccess(team_id:"` + teamID.String() + `",app_family_id:"` + appID.String() + `",level:MANAGER) { changed authorization_revision binding { role_slug resource_type resource_id } } }`

	response := executeTeamGraphQLWithSink(t, s, actor, query, sink)

	if response.Code != http.StatusOK || !mutationChanged(t, response, "grantTeamAppAccess") || len(sink.revisions) != 1 || sink.revisions[0] != 44 {
		t.Fatalf("response/revisions = %d %s / %#v", response.Code, response.Body.String(), sink.revisions)
	}
}

func TestTeamAppBindingGraphQLFragmentRequiresAccessManageBeforeAnyCall(t *testing.T) {
	teamID, appID := uuid.New(), uuid.New()
	s := &teamGraphQLTestStore{}
	query := `mutation Share {
		...Grant
		revokeTeamAppAccess(team_id:"` + teamID.String() + `",app_family_id:"` + appID.String() + `",level:READER) { changed }
	}
	fragment Grant on EngineMutation {
		grantTeamAppAccess(team_id:"` + teamID.String() + `",app_family_id:"` + appID.String() + `",level:MANAGER) { changed }
	}`

	response := executeTeamGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), query)

	if response.Code != http.StatusForbidden || s.addCalls != 0 || s.removeCalls != 0 {
		t.Fatalf("status/calls = %d/%d/%d; body=%s", response.Code, s.addCalls, s.removeCalls, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"permission":"access.manage"`) {
		t.Fatalf("structured denial missing access.manage: %s", response.Body.String())
	}
}

func TestTeamWorkspaceRoleGraphQLSetsThenClearsAtomically(t *testing.T) {
	teamID := uuid.New()
	setBinding := store.TeamBinding{ID: uuid.New(), TeamID: teamID, RoleSlug: accesscontrol.RoleViewer, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace}, CreatedAt: time.Now()}
	clearBinding := setBinding
	s := &teamGraphQLTestStore{
		addResult:   store.TeamBindingMutationResult{Binding: setBinding, AuthorizationRevision: 30, Changed: true},
		clearResult: store.TeamBindingMutationResult{Binding: clearBinding, AuthorizationRevision: 31, Changed: true},
	}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	sink := &teamRevisionSink{}
	setQuery := `mutation { setTeamWorkspaceRole(team_id:"` + teamID.String() + `",role:VIEWER) { changed binding { role_slug } } }`
	clearQuery := `mutation ClearTeamWorkspaceRole($teamId:ID!,$role:TeamWorkspaceRole) { setTeamWorkspaceRole(team_id:$teamId,role:$role) { changed binding { role_slug } } }`

	setResponse := executeTeamGraphQLWithSink(t, s, actor, setQuery, sink)
	clearResponse := executeTeamGraphQLRequest(t, s, actor, clearQuery, map[string]interface{}{"teamId": teamID.String(), "role": nil}, sink)
	assertTeamWorkspaceRoleResponses(t, setResponse, clearResponse)
	assertTeamWorkspaceRoleCalls(t, s)
	assertTeamWorkspaceRoleClearArguments(t, s, teamID, actor)
	assertTeamWorkspaceRoleRevisions(t, sink)
}

func assertTeamWorkspaceRoleResponses(t *testing.T, setResponse, clearResponse *httptest.ResponseRecorder) {
	t.Helper()
	if !mutationChanged(t, setResponse, "setTeamWorkspaceRole") || !mutationChanged(t, clearResponse, "setTeamWorkspaceRole") {
		t.Fatalf("set/clear responses = %s / %s", setResponse.Body.String(), clearResponse.Body.String())
	}
}

func assertTeamWorkspaceRoleCalls(t *testing.T, s *teamGraphQLTestStore) {
	t.Helper()
	if s.addCalls != 1 || s.lastBinding.RoleSlug != accesscontrol.RoleViewer || s.clearCalls != 1 {
		t.Fatalf("set/clear calls = add:%d binding:%#v clear:%d", s.addCalls, s.lastBinding, s.clearCalls)
	}
}

func assertTeamWorkspaceRoleClearArguments(t *testing.T, s *teamGraphQLTestStore, teamID uuid.UUID, actor accesscontrol.Actor) {
	t.Helper()
	if s.clearTeamID != teamID || s.clearWorkspaceID != actor.WorkspaceID || s.clearActor.SubjectID != actor.SubjectID || s.clearActor.CredentialID != actor.CredentialID {
		t.Fatalf("clear arguments = team:%s workspace:%s actor:%#v", s.clearTeamID, s.clearWorkspaceID, s.clearActor)
	}
}

func assertTeamWorkspaceRoleRevisions(t *testing.T, sink *teamRevisionSink) {
	t.Helper()
	if len(sink.revisions) != 2 || sink.revisions[0] != 30 || sink.revisions[1] != 31 {
		t.Fatalf("revision invalidations = %#v", sink.revisions)
	}
}

func TestTeamGraphQLAcceptsIDVariablesFromSharedClientContract(t *testing.T) {
	team := testGraphQLTeam()
	serviceID := uuid.New()
	binding := store.TeamBinding{
		ID: uuid.New(), TeamID: team.ID, RoleSlug: accesscontrol.RoleServiceUser,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, CreatedAt: time.Now(),
	}
	tests := []struct {
		name      string
		query     string
		variables map[string]interface{}
		store     *teamGraphQLTestStore
		actor     accesscontrol.Actor
	}{
		{
			name: "team query", query: `query Team($id: ID!) { team(id: $id) { id } }`,
			variables: map[string]interface{}{"id": team.ID.String()}, store: &teamGraphQLTestStore{getResult: team},
			actor: actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead),
		},
		{
			name: "resource mutation", query: `mutation Grant($teamId: ID!, $resourceId: ID!) {
				grantTeamServiceAccess(team_id: $teamId, service_id: $resourceId, level: USER) { changed }
			}`,
			variables: map[string]interface{}{"teamId": team.ID.String(), "resourceId": serviceID.String()},
			store:     &teamGraphQLTestStore{addResult: store.TeamBindingMutationResult{Binding: binding, Changed: true}},
			actor:     actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := executeTeamGraphQLWithVariables(t, test.store, test.actor, test.query, test.variables)
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestTeamAccessEditorClientQueryExecutesAgainstEngineSchema(t *testing.T) {
	team := testGraphQLTeam()
	query := `query TeamAccessEditor($id: ID!) {
		team(id: $id) { id name slug description status bindings { role_slug resource_type resource_id } }
		workspaceServicePage(limit: 100, offset: 0) { data { service_id service_name } }
		bucketSummaryPage(limit: 100, offset: 0) { items { id name } }
	}`
	actor := actorWithTeamPermissions(t,
		accesscontrol.PermissionAccessRead,
		accesscontrol.PermissionServiceRead,
		accesscontrol.PermissionBucketRead,
	)
	response := executeTeamGraphQLWithVariables(t, &teamGraphQLTestStore{getResult: team}, actor, query, map[string]interface{}{"id": team.ID.String()})
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
		t.Fatalf("editor response = %d %s", response.Code, response.Body.String())
	}
	data := responseData(t, response)
	servicePage, ok := data["workspaceServicePage"].(map[string]interface{})
	if !ok || servicePage["data"] == nil {
		t.Fatalf("workspaceServicePage = %#v, want data field", data["workspaceServicePage"])
	}
}

func TestTeamGraphQLResolvesSlugBeforePointRead(t *testing.T) {
	team := testGraphQLTeam()
	s := &teamGraphQLTestStore{
		getResult:    team,
		referenceIDs: map[string]uuid.UUID{"team:" + team.Slug: team.ID},
	}
	response := executeTeamGraphQLWithVariables(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead),
		`query Team($id:ID!){team(id:$id){id slug}}`, map[string]interface{}{"id": team.Slug})
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if len(s.referenceQueries) != 1 || s.referenceQueries[0].Kind != store.ReferenceTeam {
		t.Fatalf("slug resolution queries = %#v", s.referenceQueries)
	}
}

func TestTeamBindingGraphQLInvalidatesAuthorizationOnlyWhenChanged(t *testing.T) {
	teamID, bucketID := uuid.New(), uuid.New()
	binding := store.TeamBinding{ID: uuid.New(), TeamID: teamID, RoleSlug: accesscontrol.RoleBucketUser, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}, CreatedAt: time.Now()}
	s := &teamGraphQLTestStore{addResult: store.TeamBindingMutationResult{Binding: binding, AuthorizationRevision: 12, Changed: true}, removeResult: store.TeamBindingMutationResult{AuthorizationRevision: 12, Changed: false}}
	sink := &teamRevisionSink{}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	grant := `mutation { grantTeamBucketAccess(team_id:"` + teamID.String() + `",bucket_id:"` + bucketID.String() + `",level:USER) { changed } }`
	revoke := `mutation { revokeTeamBucketAccess(team_id:"` + teamID.String() + `",bucket_id:"` + bucketID.String() + `",level:USER) { changed binding { id } } }`

	executeTeamGraphQLWithSink(t, s, actor, grant, sink)
	response := executeTeamGraphQLWithSink(t, s, actor, revoke, sink)
	if len(sink.revisions) != 1 || sink.revisions[0] != 12 {
		t.Fatalf("revision invalidations = %#v, want [12]", sink.revisions)
	}
	bindingPayload := responseData(t, response)["revokeTeamBucketAccess"].(map[string]interface{})
	if bindingPayload["binding"] != nil {
		t.Fatalf("no-op revoke binding = %#v, want null", bindingPayload["binding"])
	}
}

func TestTeamBindingGraphQLPreservesIdempotentGrantResult(t *testing.T) {
	teamID, bucketID := uuid.New(), uuid.New()
	binding := store.TeamBinding{ID: uuid.New(), TeamID: teamID, RoleSlug: accesscontrol.RoleBucketUser, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}, CreatedAt: time.Now()}
	s := &teamGraphQLTestStore{addResult: store.TeamBindingMutationResult{Binding: binding, AuthorizationRevision: 21, Changed: true}}
	sink := &teamRevisionSink{}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	query := `mutation { grantTeamBucketAccess(team_id:"` + teamID.String() + `",bucket_id:"` + bucketID.String() + `",level:USER) { changed } }`
	first := executeTeamGraphQLWithSink(t, s, actor, query, sink)
	s.addResult = store.TeamBindingMutationResult{Binding: binding, AuthorizationRevision: 21, Changed: false}
	second := executeTeamGraphQLWithSink(t, s, actor, query, sink)

	if !mutationChanged(t, first, "grantTeamBucketAccess") || mutationChanged(t, second, "grantTeamBucketAccess") {
		t.Fatalf("idempotent responses = %s / %s", first.Body.String(), second.Body.String())
	}
	if s.addCalls != 2 || s.addSideEffects != 1 || len(sink.revisions) != 1 || sink.revisions[0] != 21 {
		t.Fatalf("calls/effects/invalidations = %d/%d/%#v", s.addCalls, s.addSideEffects, sink.revisions)
	}
}

func TestUpdateTeamGraphQLRejectsEmptyPatchWithoutRepositoryCall(t *testing.T) {
	s := &teamGraphQLTestStore{}
	response := executeTeamGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), `mutation { updateTeam(id:"`+uuid.NewString()+`",input:{}) { changed } }`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "requires at least one field") || s.updateCalls != 0 {
		t.Fatalf("response/calls = %s / %d", response.Body.String(), s.updateCalls)
	}
}

func TestCreateTeamGraphQLEmitsUserActionSpan(t *testing.T) {
	exporter := setupTestTracer(t)
	team := testGraphQLTeam()
	s := &teamGraphQLTestStore{createResults: []store.TeamMutationResult{{Team: team, Changed: true}}}
	executeTeamGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), `mutation { createTeam(input:{name:"Platform"}) { changed } }`)

	spans := exporter.GetSpans()
	for _, span := range spans {
		if span.Name == "engine.graphql.team.create" && teamSpanAttribute(span.Attributes, "user_action") == "team.create" && teamSpanAttribute(span.Attributes, "outcome") == "success" {
			return
		}
	}
	t.Fatalf("team mutation span not found: %#v", spans)
}

func executeTeamGraphQL(t *testing.T, s *teamGraphQLTestStore, actor accesscontrol.Actor, query string) *httptest.ResponseRecorder {
	return executeTeamGraphQLWithSink(t, s, actor, query, nil)
}

func executeTeamGraphQLWithSink(t *testing.T, s *teamGraphQLTestStore, actor accesscontrol.Actor, query string, sink authorizationRevisionSink) *httptest.ResponseRecorder {
	return executeTeamGraphQLRequest(t, s, actor, query, nil, sink)
}

func executeTeamGraphQLWithVariables(t *testing.T, s *teamGraphQLTestStore, actor accesscontrol.Actor, query string, variables map[string]interface{}) *httptest.ResponseRecorder {
	return executeTeamGraphQLRequest(t, s, actor, query, variables, nil)
}

func executeTeamGraphQLRequest(t *testing.T, s *teamGraphQLTestStore, actor accesscontrol.Actor, query string, variables map[string]interface{}, sink authorizationRevisionSink) *httptest.ResponseRecorder {
	t.Helper()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{SSOEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body, _ := json.Marshal(map[string]interface{}{"query": query, "variables": variables})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s, revisionSink: sink})(response, request)
	return response
}

func teamSpanAttribute(attributes []attribute.KeyValue, name string) string {
	for _, value := range attributes {
		if string(value.Key) == name {
			return value.Value.AsString()
		}
	}
	return ""
}

func actorWithTeamPermissions(t *testing.T, permissions ...accesscontrol.Permission) accesscontrol.Actor {
	t.Helper()
	workspaceID := uuid.New()
	grants := make([]accesscontrol.Grant, 0, len(permissions))
	for _, permission := range permissions {
		grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatal(err)
	}
	return accesscontrol.Actor{AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(), CredentialID: uuid.New(), Authorization: snapshot}
}

func actorForBuiltInRole(t *testing.T, roleSlug string) accesscontrol.Actor {
	t.Helper()
	for _, role := range accesscontrol.BuiltInRoles() {
		if role.Slug == roleSlug {
			return actorWithTeamPermissions(t, role.Permissions...)
		}
	}
	t.Fatal("role not found: " + roleSlug)
	return accesscontrol.Actor{}
}

func responseData(t *testing.T, response *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data, _ := payload["data"].(map[string]interface{})
	return data
}

func mutationChanged(t *testing.T, response *httptest.ResponseRecorder, field string) bool {
	t.Helper()
	payload := responseData(t, response)[field].(map[string]interface{})
	changed, _ := payload["changed"].(bool)
	return changed
}

func testGraphQLTeam() store.Team {
	now := time.Now().UTC()
	return store.Team{ID: uuid.New(), Name: "Platform", Slug: "platform", Status: store.TeamStatusActive, CreatedAt: now, UpdatedAt: now}
}
