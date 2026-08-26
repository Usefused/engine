package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type unifiedReceiptQueryStore struct {
	*workspaceTestStore
	filter store.EngineExecutionFilter
	calls  int
	event  models.EngineExecutionEvent
}

// ListEngineExecutionEventsByApp captures the exact authorized SQL inputs for child navigation.
func (s *unifiedReceiptQueryStore) ListEngineExecutionEventsByApp(_ context.Context, filter store.EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	s.calls++
	s.filter = filter
	return []models.EngineExecutionEvent{s.event}, 1, nil
}

// TestUnifiedReceiptGraphQLScopesChildrenAndRequiresAudit exercises the real shared schema and authorization gate.
func TestUnifiedReceiptGraphQLScopesChildrenAndRequiresAudit(t *testing.T) {
	for _, transport := range []string{"sdk", "mcp"} {
		for _, audit := range []bool{false, true} {
			assertUnifiedReceiptGraphQL(t, transport, audit)
		}
	}
}

// assertUnifiedReceiptGraphQL verifies a known parent ID never bypasses app/audit authorization.
func assertUnifiedReceiptGraphQL(t *testing.T, transport string, audit bool) {
	t.Helper()
	account, app, family, parent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	s := &unifiedReceiptQueryStore{workspaceTestStore: &workspaceTestStore{accountID: account, apps: map[uuid.UUID]store.App{app: {AppID: app, AppFamilyID: family, AccountID: account, Status: store.AppStatusActive}}}, event: models.EngineExecutionEvent{ID: uuid.New(), AppID: app, AppFamilyID: family, Transport: transport, ExecutionKind: "physical", ParentExecutionID: parent, UnifiedTarget: "items", ExecutionPhase: "rollback"}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	// Construction catches missing schema fields before request authorization is exercised.
	if err != nil {
		t.Fatal(err)
	}
	permissions := []accesscontrol.Permission{accesscontrol.PermissionAppRead}
	// Exact app read alone must not grant receipt or parent navigation visibility.
	if audit {
		permissions = append(permissions, accesscontrol.PermissionAuditRead)
	}
	actor := actorWithWorkspacePermissions(t, uuid.New(), permissions...)
	actor.AccountID = account
	query := `query { appExecutionEvents(app_id:"` + app.String() + `", parent_execution_id:"` + parent.String() + `",include_all_versions:true,limit:50) { total items { execution_kind parent_execution_id unified_target execution_phase unified_steps { target phase status error_code } } } }`
	body, _ := json.Marshal(map[string]any{"query": query})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s})(response, request)
	// Denied callers cannot reach the history reader even with an exact parent UUID.
	if !audit {
		if response.Code != http.StatusForbidden || s.calls != 0 {
			t.Fatal("receipt query bypassed audit permission")
		}
		return
	}
	assertUnifiedReceiptGraphQLResult(t, s, response, account, family, parent)
}

// assertUnifiedReceiptGraphQLResult checks family scope survives the all-versions child query.
func assertUnifiedReceiptGraphQLResult(t *testing.T, s *unifiedReceiptQueryStore, response *httptest.ResponseRecorder, account, family, parent uuid.UUID) {
	t.Helper()
	// Parent identity narrows authorized account/family scope; it never replaces that scope.
	if response.Code != http.StatusOK || s.calls != 1 || s.filter.AccountID != account || s.filter.AppFamilyID != family || s.filter.AppID != uuid.Nil || s.filter.ParentExecutionID != parent {
		t.Fatal("incorrect receipt query scope")
	}
	for _, expected := range []string{parent.String(), `"rollback"`, `"items"`} {
		// The wire projection must preserve child phase/target and parent correlation.
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("missing receipt metadata %s", expected)
		}
	}
}
