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

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type accessInspectionGraphQLStore struct {
	store.Store
	explanation      store.AccessExplanation
	explanationErr   error
	explanationQuery store.AccessExplanationQuery
	explanationCalls int
	auditPage        store.AuditPage
	auditErr         error
	auditQuery       store.AuditQuery
	auditCalls       int
}

func (s *accessInspectionGraphQLStore) ExplainAccess(_ context.Context, query store.AccessExplanationQuery) (store.AccessExplanation, error) {
	s.explanationCalls++
	s.explanationQuery = query
	return s.explanation, s.explanationErr
}

func (s *accessInspectionGraphQLStore) QueryAuditEvents(_ context.Context, query store.AuditQuery) (store.AuditPage, error) {
	s.auditCalls++
	s.auditQuery = query
	return s.auditPage, s.auditErr
}

func (s *accessInspectionGraphQLStore) ExportAuditEvents(context.Context, store.AuditExportQuery) ([]store.AuditExportRow, error) {
	return nil, nil
}

func TestAccessExplanationGraphQLUsesOneVisibilityAwareRepositoryCall(t *testing.T) {
	workspaceID, targetID, teamID := uuid.New(), uuid.New(), uuid.New()
	requirement := accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()}}
	repository := &accessInspectionGraphQLStore{explanation: store.AccessExplanation{
		Requirement: requirement,
		Allowed:     true,
		Sources: []store.AccessGrantSource{{
			PrincipalType: "team", PrincipalID: teamID, TeamName: "Platform", RoleSlug: "builder", Resource: requirement.Resource,
		}},
	}}
	result := executeAccessInspectionGraphQL(t, repository, controlTestOwnerActor(workspaceID), `query Explain($target:ID!,$resource:ID!){
		accessExplanation(target_subject_id:$target,permission:"service.consume",resource_type:SERVICE,resource_id:$resource){
			allowed requirement{permission resource_type resource_id} sources{principal_type principal_id team_name role_slug resource_type resource_id} missing{permission}
		}
	}`, map[string]interface{}{"target": targetID.String(), "resource": requirement.Resource.ID.String()})
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors: %#v", result.Errors)
	}
	if repository.explanationCalls != 1 {
		t.Fatalf("repository calls = %d, want 1", repository.explanationCalls)
	}
	want := store.AccessExplanationQuery{RequesterSubjectID: repository.explanationQuery.RequesterSubjectID, TargetSubjectID: targetID, Requirement: requirement}
	if repository.explanationQuery.TargetSubjectID != want.TargetSubjectID || repository.explanationQuery.Requirement != want.Requirement {
		t.Fatalf("query = %#v, want target/requirement %#v", repository.explanationQuery, want)
	}
}

func TestAccessExplanationGraphQLMakesHiddenAndMissingIndistinguishable(t *testing.T) {
	workspaceID := uuid.New()
	query := `query { accessExplanation(target_subject_id:"` + uuid.NewString() + `",permission:"service.read",resource_type:SERVICE,resource_id:"` + uuid.NewString() + `"){allowed} }`
	var messages []string
	for _, repositoryErr := range []error{store.ErrAccessExplanationHidden, errors.New("resource missing")} {
		repository := &accessInspectionGraphQLStore{explanationErr: repositoryErr}
		result := executeAccessInspectionGraphQL(t, repository, controlTestOwnerActor(workspaceID), query, nil)
		if len(result.Errors) != 1 {
			t.Fatalf("errors = %#v, want one", result.Errors)
		}
		messages = append(messages, result.Errors[0].Message)
	}
	if messages[0] != messages[1] || messages[0] != "access explanation is unavailable" {
		t.Fatalf("messages = %#v, want identical unavailable errors", messages)
	}
}

func TestAuditEventsGraphQLPassesFiltersAndOpaqueCursorInOneCall(t *testing.T) {
	workspaceID, filteredActor, recordID := uuid.New(), uuid.New(), uuid.New()
	next := &store.AuditCursor{OccurredAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), ID: uuid.New()}
	repository := &accessInspectionGraphQLStore{auditPage: store.AuditPage{Items: []store.AuditRecord{{
		ID: recordID, OccurredAt: time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC), Action: "team.update", Outcome: accesscontrol.AuditDenied,
		MissingRequirements: []accesscontrol.Requirement{{Permission: accesscontrol.PermissionAccessManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}},
		Metadata:            map[string]any{"changed_fields": []string{"name"}},
	}}, Total: 11, NextCursor: next}}
	result := executeAccessInspectionGraphQL(t, repository, controlTestOwnerActor(workspaceID), `query Audit($actor:ID!){
		auditEvents(actor_subject_id:$actor,actions:["team.update"],outcomes:[ATTEMPTED,ROLLED_BACK,CANCELLED],from:"2026-08-01T00:00:00Z",to:"2026-08-02T00:00:00Z",limit:25){
			total next_cursor items{id action outcome missing_requirements{permission resource_type resource_id} metadata}
		}
	}`, map[string]interface{}{"actor": filteredActor.String()})
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors: %#v", result.Errors)
	}
	if repository.auditCalls != 1 || repository.auditQuery.Limit != 25 || repository.auditQuery.ActorSubjectID == nil || *repository.auditQuery.ActorSubjectID != filteredActor {
		t.Fatalf("calls/query = %d/%#v", repository.auditCalls, repository.auditQuery)
	}
	if len(repository.auditQuery.Actions) != 1 || repository.auditQuery.Actions[0] != "team.update" || len(repository.auditQuery.Outcomes) != 3 || repository.auditQuery.Outcomes[0] != accesscontrol.AuditAttempted || repository.auditQuery.Outcomes[1] != accesscontrol.AuditRolledBack || repository.auditQuery.Outcomes[2] != accesscontrol.AuditCancelled {
		t.Fatalf("filters = actions %#v/outcomes %#v", repository.auditQuery.Actions, repository.auditQuery.Outcomes)
	}
	data := result.Data.(map[string]interface{})["auditEvents"].(map[string]interface{})
	cursor, err := decodeAuditCursor(data["next_cursor"].(string))
	if err != nil || cursor.ID != next.ID || !cursor.OccurredAt.Equal(next.OccurredAt) {
		t.Fatalf("cursor = %#v/error %v, want %#v", cursor, err, next)
	}
	items := data["items"].([]interface{})
	missing := items[0].(map[string]interface{})["missing_requirements"].([]interface{})
	if len(missing) != 1 || missing[0].(map[string]interface{})["permission"] != string(accesscontrol.PermissionAccessManage) {
		t.Fatalf("GraphQL missing requirements = %#v", missing)
	}
}

func TestInspectionGraphQLPolicyTraversesFragmentsAndMultipleRoots(t *testing.T) {
	workspaceID := uuid.New()
	schema, err := newMCPGraphQLSchema(nil, &accessInspectionGraphQLStore{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body := []byte(`{"query":"query Inspection { ...Access auditEvents { total } } fragment Access on EngineQuery { accessExplanation(target_subject_id:\"11111111-1111-1111-1111-111111111111\",permission:\"workspace.read\",resource_type:WORKSPACE,resource_id:\"22222222-2222-2222-2222-222222222222\"){allowed} }"}`)
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	want := map[accesscontrol.Permission]bool{accesscontrol.PermissionAccessRead: true, accesscontrol.PermissionAuditRead: true}
	if len(plan.requirements) != 2 || plan.rootFields != 2 {
		t.Fatalf("plan = requirements %#v/root fields %d", plan.requirements, plan.rootFields)
	}
	for _, requirement := range plan.requirements {
		if !want[requirement.Permission] || requirement.Resource != (accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}) {
			t.Errorf("unexpected requirement: %#v", requirement)
		}
	}
}

func TestCurrentActorAccessUsesAuthenticatedSnapshotWithoutRepositoryReads(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(17,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
	)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	actor := accesscontrol.Actor{WorkspaceID: workspaceID, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser, Authorization: snapshot}
	repository := &accessInspectionGraphQLStore{}
	result := executeAccessInspectionGraphQL(t, repository, actor, `query {
		currentActorAccess { subject_id workspace_id kind authorization_revision grants { permission resource_type resource_id } }
	}`, nil)
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors: %#v", result.Errors)
	}
	current := result.Data.(map[string]interface{})["currentActorAccess"].(map[string]interface{})
	if current["subject_id"] != actor.SubjectID.String() || current["authorization_revision"] != 17 {
		t.Fatalf("current actor = %#v", current)
	}
	grants := current["grants"].([]interface{})
	if len(grants) != 2 || repository.auditCalls != 0 || repository.explanationCalls != 0 {
		t.Fatalf("grants/repository calls = %d/%d/%d", len(grants), repository.auditCalls, repository.explanationCalls)
	}
}

func TestCurrentActorAccessPolicyRequiresAuthenticationButNoBusinessPermission(t *testing.T) {
	workspaceID := uuid.New()
	schema, err := newMCPGraphQLSchema(nil, &accessInspectionGraphQLStore{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body := []byte(`{"query":"query { currentActorAccess { authorization_revision } }"}`)
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.rootFields != 1 || len(plan.requirements) != 0 {
		t.Fatalf("plan = %#v, want one authenticated-only root", plan)
	}

	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema)(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", response.Code)
	}

	snapshot, err := accesscontrol.NewAuthorizationSnapshot(3)
	if err != nil {
		t.Fatalf("empty snapshot: %v", err)
	}
	actor := accesscontrol.Actor{WorkspaceID: workspaceID, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser, Authorization: snapshot}
	request = httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response = httptest.NewRecorder()
	mcpGraphQLHandler(schema)(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authorization_revision":3`) {
		t.Fatalf("authenticated response = %d/%s, want current actor access", response.Code, response.Body.String())
	}
}

func TestInspectionGraphQLPreflightDeniesForgedFragmentBeforeAnyRepositoryCall(t *testing.T) {
	workspaceID := uuid.New()
	repository := &accessInspectionGraphQLStore{}
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionAccessRead)
	body := `{"query":"query Forged { ...Audit } fragment Audit on EngineQuery { disguised: auditEvents { total } }"}`
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	mcpGraphQLHandler(schema)(response, request)

	if response.Code != http.StatusForbidden || repository.auditCalls != 0 || repository.explanationCalls != 0 {
		t.Fatalf("status/audit/explanation calls = %d/%d/%d; body=%s", response.Code, repository.auditCalls, repository.explanationCalls, response.Body.String())
	}
	var denial struct {
		Missing []struct {
			Permission accesscontrol.Permission `json:"permission"`
		} `json:"missing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil {
		t.Fatalf("decode denial: %v", err)
	}
	if len(denial.Missing) != 1 || denial.Missing[0].Permission != accesscontrol.PermissionAuditRead {
		t.Fatalf("missing = %#v, want audit.read", denial.Missing)
	}
}

func executeAccessInspectionGraphQL(t *testing.T, repository *accessInspectionGraphQLStore, actor accesscontrol.Actor, query string, variables map[string]interface{}) *graphql.Result {
	t.Helper()
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	return graphql.Do(graphql.Params{Schema: schema, RequestString: query, VariableValues: variables, Context: accesscontrol.ContextWithActor(context.Background(), actor)})
}
