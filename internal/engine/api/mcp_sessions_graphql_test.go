package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type mcpSessionGraphQLTestStore struct {
	*workspaceTestStore
	page         store.MCPSessionPage
	calls        int
	account, app uuid.UUID
	after        string
	first        int
}

// ListMCPSessions records the authorized query boundary without fabricating additional lookups.
func (s *mcpSessionGraphQLTestStore) ListMCPSessions(_ context.Context, account, app uuid.UUID, after string, first int) (store.MCPSessionPage, error) {
	s.calls++
	s.account, s.app, s.after, s.first = account, app, after, first
	return s.page, nil
}

// TestMCPSessionsGraphQLPermissionMatrix requires both read capabilities before any provenance query executes.
func TestMCPSessionsGraphQLPermissionMatrix(t *testing.T) {
	for _, permissions := range [][]accesscontrol.Permission{nil, {accesscontrol.PermissionAppRead}, {accesscontrol.PermissionAuditRead}, {accesscontrol.PermissionAppRead, accesscontrol.PermissionAuditRead}} {
		assertMCPSessionGraphQLPermissions(t, permissions)
	}
}

// assertMCPSessionGraphQLPermissions uses the real authorization middleware and schema for one grant combination.
func assertMCPSessionGraphQLPermissions(t *testing.T, permissions []accesscontrol.Permission) {
	t.Helper()
	account, app, family := uuid.New(), uuid.New(), uuid.New()
	s := &mcpSessionGraphQLTestStore{workspaceTestStore: &workspaceTestStore{accountID: account, apps: map[uuid.UUID]store.App{app: {AppID: app, AppFamilyID: family, AccountID: account, Status: store.AppStatusActive}}}, page: store.MCPSessionPage{Items: []models.MCPSession{{ID: uuid.New(), SessionID: "synthetic-session", ClientName: "Example Agent", ClientVersion: "1", InitialClientIP: "192.0.2.2", StartedAt: time.Now(), LastActivityAt: time.Now()}}, NextCursor: "next-page", HasMore: true}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	// Schema construction verifies the new field and permission policy are wired together.
	if err != nil {
		t.Fatal(err)
	}
	actor := actorWithWorkspacePermissions(t, uuid.New(), permissions...)
	actor.AccountID = account
	query := `query { mcpSessions(app_id:"` + app.String() + `",after:"previous-page",first:2) { items { client_name client_version initial_client_ip } next_cursor has_more } }`
	body, _ := json.Marshal(map[string]any{"query": query})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s})(response, request)
	// Missing either capability must stop execution before the history reader is invoked.
	if len(permissions) != 2 {
		// Authorization rejects the request before resolving private network provenance.
		if response.Code != http.StatusForbidden || s.calls != 0 {
			t.Fatalf("denied request reached history: %d/%d", response.Code, s.calls)
		}
		return
	}
	assertMCPSessionGraphQLQuery(t, s, response, account, app)
	for _, expected := range []string{"Example Agent", "192.0.2.2", "next-page"} {
		// The field projection must remain complete for client and network provenance.
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("missing projected %q", expected)
		}
	}
}

// assertMCPSessionGraphQLQuery keeps the authorized account/app/cursor tuple exact through GraphQL adaptation.
func assertMCPSessionGraphQLQuery(t *testing.T, s *mcpSessionGraphQLTestStore, response *httptest.ResponseRecorder, account, app uuid.UUID) {
	t.Helper()
	// A successful status alone cannot prove tenant scope or bounded paging reached storage.
	if response.Code != http.StatusOK || s.calls != 1 || s.account != account || s.app != app || s.after != "previous-page" || s.first != 2 {
		t.Fatalf("authorized query mismatch: %d/%d %s", response.Code, s.calls, response.Body.String())
	}
}

// TestMCPSessionsGraphQLCrossAccountDenial blocks known app IDs before querying session metadata.
func TestMCPSessionsGraphQLCrossAccountDenial(t *testing.T) {
	account, app := uuid.New(), uuid.New()
	s := &mcpSessionGraphQLTestStore{workspaceTestStore: &workspaceTestStore{accountID: account, apps: map[uuid.UUID]store.App{app: {AppID: app, AppFamilyID: uuid.New(), AccountID: uuid.New()}}}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	// Cross-account denial is tested through the same complete schema as a permitted query.
	if err != nil {
		t.Fatal(err)
	}
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionAppRead, accesscontrol.PermissionAuditRead)
	actor.AccountID = account
	body, _ := json.Marshal(map[string]any{"query": `query { mcpSessions(app_id:"` + app.String() + `") { items { initial_client_ip } } }`})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s})(response, request)
	// Knowing another tenant's immutable app ID cannot disclose even the first network metadata row.
	if s.calls != 0 || response.Code == http.StatusOK {
		t.Fatalf("cross-account request passed: %d/%d", response.Code, s.calls)
	}
}
