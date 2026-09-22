package api

import (
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"net/http"
	"testing"
)

// TestPromptClassifierRequiresCataloguePermission proves the Engine bridge fails before provider dispatch without catalogue access.
func TestPromptClassifierRequiresCataloguePermission(t *testing.T) {
	s := &workspaceTestStore{}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	query := `query { classifyPromptOperation(service_id:"` + uuid.NewString() + `",version:"v1",query:"show bills") }`
	denied := doAuthorizedGraphQLRequest(t, handler, actorWithWorkspacePermissions(t, uuid.New()), query)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unprivileged status=%d", denied.Code)
	} // Authentication alone is insufficient for paid discovery.
	allowed := doAuthorizedGraphQLRequest(t, handler, actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionCatalogueRead), query)
	if allowed.Code != http.StatusOK {
		t.Fatalf("catalogue reader status=%d body=%s", allowed.Code, allowed.Body.String())
	} // An authorized caller may reach the resolver even before workspace activation.
}
