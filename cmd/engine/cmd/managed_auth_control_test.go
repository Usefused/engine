package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestManagedAuthDisableRequiresWorkspaceUpdate proves read access cannot turn delegation off.
func TestManagedAuthDisableRequiresWorkspaceUpdate(t *testing.T) {
	workspace := uuid.New()
	reader := actorWithGrants(t, workspace, accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}})
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, requestWithActor(t, http.MethodDelete, "/workspace/managed-auth", reader))
	assertControlDenial(t, denied, http.StatusForbidden, "permission_denied", 1)
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, requestWithActor(t, http.MethodDelete, "/workspace/managed-auth", ownerActor(t, workspace)))
	require.Equal(t, http.StatusNoContent, allowed.Code)
}
