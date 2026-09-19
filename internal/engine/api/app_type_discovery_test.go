package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestNarrowAppReadCanListAnEmptyCatalogue separates permission to query from existence of matching rows.
func TestNarrowAppReadCanListAnEmptyCatalogue(t *testing.T) {
	workspaceID := uuid.New()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, accesscontrol.Grant{Permission: accesscontrol.PermissionAppMCPRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
	// Only MCP read is granted; a broad fixture would conceal the empty-scope bug.
	if err != nil {
		t.Fatal(err)
	}
	actor := accesscontrol.Actor{AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(), Authorization: snapshot}
	repository := &artifactReferenceGraphQLTestStore{workspaceTestStore: &workspaceTestStore{accountID: actor.AccountID}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, repository, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"), nil)
	// The catalogue fixture must implement the same read repository as production.
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { apps(limit: 10, offset: 0) { total items { app_id } } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: repository})(response, request)
	// A valid empty result must not invent a missing generic app.read permission.
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) || !strings.Contains(response.Body.String(), `"total":0`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
