package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type userGraphQLTestStore struct {
	store.Store
	users              []store.User
	members            []store.TeamMember
	total              int
	listOptions        store.UserListOptions
	lastActor          store.MutationActor
	lastCreate         store.CreateUserInput
	lastPatch          store.UserPatch
	lastTeamID         uuid.UUID
	lastUserID         uuid.UUID
	lastCredentialID   uuid.UUID
	lastMembershipRole store.MembershipRole
	calls              map[string]int
	userResult         store.User
	userMutation       store.UserMutationResult
	membershipMutation store.MembershipMutationResult
	issuedCredential   store.IssuedControlCredential
	credentialMutation store.CredentialMutationResult
	effectiveGrants    []store.EffectiveAccessGrant
	effectiveRevision  int64
	err                error
}

func (s *userGraphQLTestStore) called(operation string) { s.ensureCalls(); s.calls[operation]++ }
func (s *userGraphQLTestStore) ensureCalls() {
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
}
func (s *userGraphQLTestStore) callCount() int {
	total := 0
	for _, count := range s.calls {
		total += count
	}
	return total
}

func (s *userGraphQLTestStore) CreateUser(_ context.Context, input store.CreateUserInput) (store.UserMutationResult, error) {
	s.called("create")
	s.lastActor, s.lastCreate = input.Actor, input
	return s.userMutation, s.err
}
func (s *userGraphQLTestStore) GetUser(_ context.Context, userID uuid.UUID) (store.User, error) {
	s.called("get")
	s.lastUserID = userID
	return s.userResult, s.err
}
func (s *userGraphQLTestStore) ListUsers(_ context.Context, options store.UserListOptions) ([]store.User, int, error) {
	s.called("list")
	s.listOptions = options
	return s.users, s.total, s.err
}
func (s *userGraphQLTestStore) UpdateUser(_ context.Context, userID uuid.UUID, patch store.UserPatch) (store.UserMutationResult, error) {
	s.called("update")
	s.lastUserID, s.lastActor, s.lastPatch = userID, patch.Actor, patch
	return s.userMutation, s.err
}
func (s *userGraphQLTestStore) SuspendUser(_ context.Context, userID uuid.UUID, actor store.MutationActor) (store.UserMutationResult, error) {
	s.called("suspend")
	s.lastUserID, s.lastActor = userID, actor
	return s.userMutation, s.err
}
func (s *userGraphQLTestStore) ReactivateUser(_ context.Context, userID uuid.UUID, actor store.MutationActor) (store.UserMutationResult, error) {
	s.called("reactivate")
	s.lastUserID, s.lastActor = userID, actor
	return s.userMutation, s.err
}
func (s *userGraphQLTestStore) AddTeamMember(_ context.Context, input store.TeamMemberMutation) (store.MembershipMutationResult, error) {
	s.called("add-direct")
	s.lastTeamID, s.lastUserID, s.lastMembershipRole, s.lastActor = input.TeamID, input.UserID, input.Role, input.Actor
	return s.membershipMutation, s.err
}
func (s *userGraphQLTestStore) AddTeamMemberByEmail(_ context.Context, input store.AddTeamMemberByEmailInput) (store.MembershipMutationResult, error) {
	s.called("add")
	s.lastTeamID, s.lastMembershipRole, s.lastActor = input.TeamID, input.Role, input.Actor
	s.lastCreate.Email, s.lastCreate.DisplayName = input.Email, input.DisplayName
	return s.membershipMutation, s.err
}
func (s *userGraphQLTestStore) RemoveTeamMember(_ context.Context, teamID, userID uuid.UUID, actor store.MutationActor) (store.MembershipMutationResult, error) {
	s.called("remove")
	s.lastTeamID, s.lastUserID, s.lastActor = teamID, userID, actor
	return s.membershipMutation, s.err
}
func (s *userGraphQLTestStore) ListTeamMembers(_ context.Context, teamID uuid.UUID, options store.UserListOptions) ([]store.TeamMember, int, error) {
	s.called("members")
	s.lastTeamID, s.listOptions = teamID, options
	return s.members, s.total, s.err
}
func (s *userGraphQLTestStore) GetUserEffectiveAccess(_ context.Context, userID uuid.UUID) ([]store.EffectiveAccessGrant, int64, error) {
	s.called("effective")
	s.lastUserID = userID
	return s.effectiveGrants, s.effectiveRevision, s.err
}
func (s *userGraphQLTestStore) IssueUserControlCredential(_ context.Context, input store.IssueCredentialInput) (store.IssuedControlCredential, error) {
	s.called("issue")
	s.lastUserID, s.lastActor = input.UserID, input.Actor
	return s.issuedCredential, s.err
}
func (s *userGraphQLTestStore) ListUserControlCredentials(_ context.Context, userID uuid.UUID) ([]store.ControlCredential, error) {
	s.called("credentials")
	s.lastUserID = userID
	return nil, s.err
}
func (s *userGraphQLTestStore) RevokeUserControlCredential(_ context.Context, userID, credentialID uuid.UUID, actor store.MutationActor) (store.CredentialMutationResult, error) {
	s.called("revoke")
	s.lastUserID, s.lastCredentialID, s.lastActor = userID, credentialID, actor
	return s.credentialMutation, s.err
}

func TestUsersGraphQLReadsAggregatesWithoutNestedRepositoryCalls(t *testing.T) {
	user, membership, credential := testGraphQLUser()
	lastUsed := credential.CreatedAt.Add(time.Minute)
	credential.LastUsedAt = &lastUsed
	user.Credentials[0] = credential
	user.MembershipsTruncated = true
	user.CredentialsTruncated = true
	s := &userGraphQLTestStore{users: []store.User{user}, total: 1}
	response := executeUserGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), `query {
		users(search:"ada",limit:7,offset:2,include_suspended:true) {
			total items { id email display_name status memberships_truncated credentials_truncated memberships { team_id team_name membership_role } credentials { id name key_prefix last_used_at } }
		}
	}`, nil, nil)

	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if s.calls["list"] != 1 || s.callCount() != 1 || s.listOptions.Search != "ada" || s.listOptions.Limit != 7 || s.listOptions.Offset != 2 || len(s.listOptions.Statuses) != 3 || !s.listOptions.IncludeChildren {
		t.Fatalf("repository calls/options = %#v / %#v", s.calls, s.listOptions)
	}
	body := responseData(t, response)["users"].(map[string]interface{})
	item := body["items"].([]interface{})[0].(map[string]interface{})
	if item["id"] != user.ID.String() || item["status"] != "ACTIVE" || item["memberships_truncated"] != true || item["credentials_truncated"] != true {
		t.Fatalf("user projection = %#v", item)
	}
	projectedMembership := item["memberships"].([]interface{})[0].(map[string]interface{})
	projectedCredential := item["credentials"].([]interface{})[0].(map[string]interface{})
	if projectedMembership["team_id"] != membership.TeamID.String() || projectedCredential["id"] != credential.ID.String() || projectedCredential["last_used_at"] != graphQLUserTime(lastUsed) {
		t.Fatalf("aggregate projection = %#v / %#v", projectedMembership, projectedCredential)
	}
}

func TestUsersGraphQLSummaryDoesNotHydrateNestedCollections(t *testing.T) {
	user, _, _ := testGraphQLUser()
	s := &userGraphQLTestStore{users: []store.User{user}, total: 1}
	response := executeUserGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), `query {
		users { total items { id email display_name status } }
	}`, nil, nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if s.calls["list"] != 1 || s.listOptions.IncludeChildren {
		t.Fatalf("summary list options/calls = %#v/%#v", s.listOptions, s.calls)
	}
}

func TestUserGraphQLReadContractsExecuteAgainstSchema(t *testing.T) {
	user, membership, _ := testGraphQLUser()
	teamMember := store.TeamMember{UserID: user.ID, Email: user.Email, DisplayName: user.DisplayName, UserStatus: user.Status, MembershipRole: membership.Role, CreatedAt: membership.CreatedAt}
	grant := store.EffectiveAccessGrant{
		Permission: accesscontrol.PermissionServiceConsume,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()},
		RoleSlug:   accesscontrol.RoleServiceUser, SourceType: "team", SourceID: membership.TeamID, SourceDisplayName: membership.TeamName,
	}
	tests := []struct {
		name, query string
		variables   map[string]interface{}
		store       *userGraphQLTestStore
	}{
		{name: "user", query: `query User($id:ID!){user(id:$id){id status credentials{id}}}`, variables: map[string]interface{}{"id": user.ID.String()}, store: &userGraphQLTestStore{userResult: user}},
		{name: "members", query: `query Members($teamId:ID!,$limit:Int!,$offset:Int!){teamMembers(team_id:$teamId,limit:$limit,offset:$offset){total items{user_id status membership_role}}}`, variables: map[string]interface{}{"teamId": membership.TeamID.String(), "limit": 20, "offset": 0}, store: &userGraphQLTestStore{members: []store.TeamMember{teamMember}, total: 1}},
		{name: "effective", query: `query Access($userId:ID!){userEffectiveAccess(user_id:$userId){user_id authorization_revision grants{permission resource_type resource_id role_slug source_type source_id source_display_name}}}`, variables: map[string]interface{}{"userId": user.ID.String()}, store: &userGraphQLTestStore{effectiveGrants: []store.EffectiveAccessGrant{grant}, effectiveRevision: 9}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := executeUserGraphQL(t, test.store, actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead), test.query, test.variables, nil)
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestUserGraphQLAllOperationsFailClosedBeforeRepositoryCalls(t *testing.T) {
	userID, teamID, credentialID := uuid.New(), uuid.New(), uuid.New()
	reads := []string{
		`query { users { total } }`,
		`query { user(id:"` + userID.String() + `") { id } }`,
		`query { userEffectiveAccess(user_id:"` + userID.String() + `") { user_id } }`,
		`query { teamMembers(team_id:"` + teamID.String() + `") { total } }`,
	}
	mutations := []string{
		`mutation { createUser(input:{email:"a@example.com",display_name:"A"}) { changed } }`,
		`mutation { updateUser(id:"` + userID.String() + `",input:{display_name:"A"}) { changed } }`,
		`mutation { suspendUser(id:"` + userID.String() + `") { changed } }`,
		`mutation { reactivateUser(id:"` + userID.String() + `") { changed } }`,
		`mutation { addTeamMember(team_id:"` + teamID.String() + `",email:"a@example.com") { changed } }`,
		`mutation { removeTeamMember(team_id:"` + teamID.String() + `",user_id:"` + userID.String() + `") { changed } }`,
		`mutation { issueUserCredential(user_id:"` + userID.String() + `",name:"Laptop") { changed } }`,
		`mutation { revokeUserCredential(user_id:"` + userID.String() + `",credential_id:"` + credentialID.String() + `") { changed } }`,
	}
	for _, query := range append(reads, mutations...) {
		s := &userGraphQLTestStore{}
		actor := actorWithTeamPermissions(t)
		if strings.HasPrefix(query, "mutation") {
			actor = actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead)
		}
		response := executeUserGraphQL(t, s, actor, query, nil, nil)
		if response.Code != http.StatusForbidden || s.callCount() != 0 {
			t.Fatalf("response/calls = %d/%d for %s: %s", response.Code, s.callCount(), query, response.Body.String())
		}
	}
}

func TestUserGraphQLMissingActorReturns401WithoutRepositoryCall(t *testing.T) {
	s := &userGraphQLTestStore{}
	response := executeUserGraphQLWithoutActor(t, s, `query { users { total } }`)
	if response.Code != http.StatusUnauthorized || s.callCount() != 0 {
		t.Fatalf("response/calls = %d/%d: %s", response.Code, s.callCount(), response.Body.String())
	}
}

func TestUpdateUserGraphQLRejectsEmptyPatchWithoutRepositoryCall(t *testing.T) {
	s := &userGraphQLTestStore{}
	query := `mutation { updateUser(id:"` + uuid.NewString() + `",input:{}) { changed } }`
	response := executeUserGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), query, nil, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "requires at least one field") || s.callCount() != 0 {
		t.Fatalf("response/calls = %d %s / %d", response.Code, response.Body.String(), s.callCount())
	}
}

func TestUserGraphQLMutationContractsInvalidateAndPropagateActor(t *testing.T) {
	exporter := setupTestTracer(t)
	user, membership, credential := testGraphQLUser()
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage)
	sink := &teamRevisionSink{}
	tests := []struct {
		name, query string
		variables   map[string]interface{}
		store       *userGraphQLTestStore
		field       string
	}{
		{name: "create", field: "createUser", query: `mutation Create($input:CreateUserInput!){createUser(input:$input){user{id status} authorization_revision changed}}`, variables: map[string]interface{}{"input": map[string]interface{}{"email": user.Email, "display_name": user.DisplayName}}, store: &userGraphQLTestStore{userMutation: store.UserMutationResult{User: user, AuthorizationRevision: 2, Changed: true}}},
		{name: "update", field: "updateUser", query: `mutation Update($id:ID!,$input:UpdateUserInput!){updateUser(id:$id,input:$input){changed}}`, variables: map[string]interface{}{"id": user.ID.String(), "input": map[string]interface{}{"display_name": "Ada L."}}, store: &userGraphQLTestStore{userMutation: store.UserMutationResult{User: user, AuthorizationRevision: 3, Changed: true}}},
		{name: "suspend", field: "suspendUser", query: `mutation Suspend($id:ID!){suspendUser(id:$id){changed}}`, variables: map[string]interface{}{"id": user.ID.String()}, store: &userGraphQLTestStore{userMutation: store.UserMutationResult{User: user, AuthorizationRevision: 4, Changed: true}}},
		{name: "reactivate", field: "reactivateUser", query: `mutation Reactivate($id:ID!){reactivateUser(id:$id){changed}}`, variables: map[string]interface{}{"id": user.ID.String()}, store: &userGraphQLTestStore{userMutation: store.UserMutationResult{User: user, AuthorizationRevision: 5, Changed: true}}},
		{name: "add", field: "addTeamMember", query: `mutation Add($teamId:ID!,$email:String!,$role:TeamMembershipRole){addTeamMember(team_id:$teamId,email:$email,membership_role:$role){membership{user_id membership_role} changed}}`, variables: map[string]interface{}{"teamId": membership.TeamID.String(), "email": user.Email, "role": "MANAGER"}, store: &userGraphQLTestStore{membershipMutation: store.MembershipMutationResult{User: user, Membership: membership, AuthorizationRevision: 6, Changed: true}}},
		{name: "remove", field: "removeTeamMember", query: `mutation Remove($teamId:ID!,$userId:ID!){removeTeamMember(team_id:$teamId,user_id:$userId){changed}}`, variables: map[string]interface{}{"teamId": membership.TeamID.String(), "userId": user.ID.String()}, store: &userGraphQLTestStore{membershipMutation: store.MembershipMutationResult{User: user, Membership: membership, AuthorizationRevision: 7, Changed: true}}},
		{name: "issue", field: "issueUserCredential", query: `mutation Issue($userId:ID!,$name:String!){issueUserCredential(user_id:$userId,name:$name){credential{id key_prefix} secret changed}}`, variables: map[string]interface{}{"userId": user.ID.String(), "name": "Laptop"}, store: &userGraphQLTestStore{issuedCredential: store.IssuedControlCredential{Credential: credential, RawKey: "fsk_once", AuthorizationRevision: 8, Changed: true}}},
		{name: "revoke", field: "revokeUserCredential", query: `mutation Revoke($userId:ID!,$credentialId:ID!){revokeUserCredential(user_id:$userId,credential_id:$credentialId){credential{id revoked_at} changed}}`, variables: map[string]interface{}{"userId": user.ID.String(), "credentialId": credential.ID.String()}, store: &userGraphQLTestStore{credentialMutation: store.CredentialMutationResult{Credential: credential, AuthorizationRevision: 9, Changed: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			localSink := &teamRevisionSink{}
			response := executeUserGraphQL(t, test.store, actor, test.query, test.variables, localSink)
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) || !mutationChanged(t, response, test.field) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if len(localSink.revisions) != 1 || test.store.lastActor.SubjectID != actor.SubjectID || test.store.lastActor.CredentialID != actor.CredentialID || test.store.lastActor.RequestID == "" || test.store.lastActor.TraceID == "" {
				t.Fatalf("revision/actor = %#v / %#v", localSink.revisions, test.store.lastActor)
			}
			sink.revisions = append(sink.revisions, localSink.revisions...)
		})
	}
	if len(sink.revisions) != len(tests) {
		t.Fatalf("all mutation revisions = %#v", sink.revisions)
	}
	for _, span := range exporter.GetSpans() {
		if span.Name == "engine.graphql.user.create" && teamSpanAttribute(span.Attributes, "user_action") == "user.create" && teamSpanAttribute(span.Attributes, "outcome") == "success" {
			return
		}
	}
	t.Fatal("user mutation OTEL span was not exported")
}

func TestIssuedCredentialSecretIsOneTimeOnlySchemaField(t *testing.T) {
	user, _, credential := testGraphQLUser()
	s := &userGraphQLTestStore{
		userResult:       user,
		issuedCredential: store.IssuedControlCredential{Credential: credential, RawKey: "fsk_one_time_secret", AuthorizationRevision: 2, Changed: true},
	}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead, accesscontrol.PermissionAccessManage)
	issue := executeUserGraphQL(t, s, actor, `mutation { issueUserCredential(user_id:"`+user.ID.String()+`",name:"Laptop"){secret} }`, nil, &teamRevisionSink{})
	read := executeUserGraphQL(t, s, actor, `query { user(id:"`+user.ID.String()+`"){credentials{id name key_prefix}} }`, nil, nil)
	invalid := executeUserGraphQL(t, s, actor, `query { user(id:"`+user.ID.String()+`"){credentials{secret}} }`, nil, nil)
	if !strings.Contains(issue.Body.String(), "fsk_one_time_secret") || strings.Contains(read.Body.String(), "fsk_one_time_secret") {
		t.Fatalf("issue/read responses = %s / %s", issue.Body.String(), read.Body.String())
	}
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "invalid_graphql_request") || s.calls["get"] != 1 {
		t.Fatalf("invalid metadata query/calls = %s / %#v", invalid.Body.String(), s.calls)
	}
}

func TestUserGraphQLSafeErrorsAndNoInvalidationOnFailure(t *testing.T) {
	user, _, _ := testGraphQLUser()
	s := &userGraphQLTestStore{err: store.ErrLastEffectiveOwner}
	sink := &teamRevisionSink{}
	response := executeUserGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), `mutation { suspendUser(id:"`+user.ID.String()+`"){changed} }`, nil, sink)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "at least one effective workspace Owner must remain") || len(sink.revisions) != 0 {
		t.Fatalf("response/revisions = %d %s / %#v", response.Code, response.Body.String(), sink.revisions)
	}
	if strings.Contains(response.Body.String(), user.Email) || !errors.Is(s.err, store.ErrLastEffectiveOwner) {
		t.Fatal("safe error leaked user data")
	}
}

func TestUserGraphQLNoOpMutationDoesNotInvalidateAuthorization(t *testing.T) {
	user, _, credential := testGraphQLUser()
	s := &userGraphQLTestStore{credentialMutation: store.CredentialMutationResult{AuthorizationRevision: 12, Changed: false}}
	sink := &teamRevisionSink{}
	query := `mutation { revokeUserCredential(user_id:"` + user.ID.String() + `",credential_id:"` + credential.ID.String() + `") { credential { id } changed } }`
	response := executeUserGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), query, nil, sink)
	if response.Code != http.StatusOK || mutationChanged(t, response, "revokeUserCredential") || len(sink.revisions) != 0 {
		t.Fatalf("response/revisions = %d %s / %#v", response.Code, response.Body.String(), sink.revisions)
	}
	payload := responseData(t, response)["revokeUserCredential"].(map[string]interface{})
	if payload["credential"] != nil {
		t.Fatalf("no-op credential = %#v, want null", payload["credential"])
	}
}

func TestRemoveTeamMemberGraphQLRepeatedRemoveReturnsNullMembership(t *testing.T) {
	user, membership, _ := testGraphQLUser()
	// The repository keeps the requested identities on an idempotent remove,
	// but an absent persisted membership has no role or creation timestamp.
	s := &userGraphQLTestStore{membershipMutation: store.MembershipMutationResult{
		User: store.User{ID: user.ID}, Membership: store.TeamMembership{TeamID: membership.TeamID},
		AuthorizationRevision: 15, Changed: false,
	}}
	sink := &teamRevisionSink{}
	query := `mutation { removeTeamMember(team_id:"` + membership.TeamID.String() + `",user_id:"` + user.ID.String() + `") { membership { user_id membership_role created_at } changed } }`
	response := executeUserGraphQL(t, s, actorWithTeamPermissions(t, accesscontrol.PermissionAccessManage), query, nil, sink)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) || mutationChanged(t, response, "removeTeamMember") || len(sink.revisions) != 0 {
		t.Fatalf("response/revisions = %d %s / %#v", response.Code, response.Body.String(), sink.revisions)
	}
	payload := responseData(t, response)["removeTeamMember"].(map[string]interface{})
	if payload["membership"] != nil {
		t.Fatalf("repeated remove membership = %#v, want null", payload["membership"])
	}
}

func executeUserGraphQL(t *testing.T, s *userGraphQLTestStore, actor accesscontrol.Actor, query string, variables map[string]interface{}, sink authorizationRevisionSink) *httptest.ResponseRecorder {
	t.Helper()
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body, _ := json.Marshal(map[string]interface{}{"query": query, "variables": variables})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "request-user-test")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler := middleware.RequestID(mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s, revisionSink: sink}))
	handler.ServeHTTP(response, request)
	return response
}

func executeUserGraphQLWithoutActor(t *testing.T, s *userGraphQLTestStore, query string) *httptest.ResponseRecorder {
	t.Helper()
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body, _ := json.Marshal(map[string]interface{}{"query": query})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema)(response, request)
	return response
}

func testGraphQLUser() (store.User, store.TeamMembership, store.ControlCredential) {
	now := time.Now().UTC()
	membership := store.TeamMembership{TeamID: uuid.New(), TeamName: "Platform", TeamSlug: "platform", TeamStatus: store.TeamStatusActive, Role: store.MembershipRoleMember, CreatedAt: now}
	credential := store.ControlCredential{ID: uuid.New(), UserID: uuid.New(), Name: "Laptop", KeyPrefix: "fsk_1234", CreatedAt: now}
	user := store.User{
		ID: credential.UserID, Email: "Ada@Example.com", DisplayName: "Ada", Status: store.UserStatusActive,
		Memberships: []store.TeamMembership{membership}, Credentials: []store.ControlCredential{credential}, CreatedAt: now, UpdatedAt: now,
	}
	return user, membership, credential
}
