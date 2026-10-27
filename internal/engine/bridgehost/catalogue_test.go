package bridgehost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// catalogueRouter uses the normal control handlers with explicit test workspace authority.
func catalogueRouter(t *testing.T, repo *Repository, actor accesscontrol.Actor) http.Handler {
	t.Helper()
	var err error
	actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(1, accesscontrol.Grant{Permission: accesscontrol.PermissionBridgeRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}, accesscontrol.Grant{Permission: accesscontrol.PermissionBridgeManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
	require.NoError(t, err)
	router := chi.NewRouter()
	// The fixture injects only its own workspace; handlers retain their actual RBAC checks.
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(accesscontrol.ContextWithActor(r.Context(), actor)))
		})
	})
	New(repo.Pool, nil).MountHTTP(router, "http://127.0.0.1:8080", "http://127.0.0.1:50051")
	return router
}

// catalogueResponse exercises the public JSON projection rather than duplicating its database query.
func catalogueResponse(t *testing.T, router http.Handler, path string) map[string]json.RawMessage {
	t.Helper()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
	require.Equal(t, 200, response.Code, response.Body.String())
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	return body
}

// TestWorkspaceCatalogueIncludesUnresolvedOfflineProfiles ensures discovery does not depend on current execution availability.
func TestWorkspaceCatalogueIncludesUnresolvedOfflineProfiles(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, _ := pairingFixture(t, repo, actor)
	require.NoError(t, repo.PutManifest(t.Context(), p, eventManifest(t, "1", false)))
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "sensors", "new")))
	router := catalogueRouter(t, repo, actor)
	body := catalogueResponse(t, router, "/bridges/"+p.ID+"/manifests")
	var manifests []struct {
		Profile  string           `json:"profile_id"`
		State    string           `json:"state"`
		Hash     string           `json:"manifest_hash"`
		Document ManifestDocument `json:"document"`
	}
	require.NoError(t, json.Unmarshal(body["manifests"], &manifests))
	require.Len(t, manifests, 2)
	require.Equal(t, "new", manifests[0].Profile)
	require.Equal(t, "offline", manifests[0].State)
	require.Empty(t, manifests[0].Document.Operations)
	require.Equal(t, "online", manifests[1].State)
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t)))
	body = catalogueResponse(t, router, "/bridges/"+p.ID+"/manifests")
	require.JSONEq(t, `[]`, string(body["manifests"]))
}

// TestWorkspacePairingRequiresExplicitConsent rejects the old allowlist request and advertises the negotiated replacement.
func TestWorkspacePairingRequiresExplicitConsent(t *testing.T) {
	repo, actor := databaseFixture(t)
	router := catalogueRouter(t, repo, actor)
	discovery := catalogueResponse(t, router, "/bridges/discovery")
	require.JSONEq(t, `"workspace"`, string(discovery["scope"]))
	require.JSONEq(t, `1`, string(discovery["protocol_minor"]))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/bridges/pairings", strings.NewReader(`{"installation_id":"11111111-1111-4111-8111-111111111111","name":"Legacy","profile_ids":["sensors"]}`)))
	require.Equal(t, 400, response.Code)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/bridges/pairings", strings.NewReader(`{"installation_id":"11111111-1111-4111-8111-111111111111","name":"Workspace","scope":"workspace"}`)))
	require.Equal(t, 201, response.Code)
	var receipt struct {
		Scope string `json:"scope"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
	require.Equal(t, "workspace", receipt.Scope)
}
