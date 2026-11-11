package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TestHostedUpgradeAuthorizationAndProxy verifies permission enforcement before the real Registry proxy swaps credentials.
func TestHostedUpgradeAuthorizationAndProxy(t *testing.T) {
	workspace := uuid.New()
	for _, manage := range []bool{false, true} {
		// Read-only and account-management actors exercise the same installed route and middleware.
		t.Run(map[bool]string{false: "reader", true: "manager"}[manage], func(t *testing.T) {
			calls := 0
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/account/engine/upgrade" || r.Header.Get("Authorization") != "Bearer server-license" || r.Header.Get("Idempotency-Key") != "upgrade-key" {
					t.Error("proxy did not preserve intent and replace local credentials")
				} // Browser identity must stop at Engine.
				w.WriteHeader(202)
				_, _ = w.Write([]byte(`{"status":"upgrading"}`))
			}))
			defer registry.Close()
			grants := []accesscontrol.Grant{{Permission: accesscontrol.PermissionAccountRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}}}
			if manage {
				grants = append(grants, accesscontrol.Grant{Permission: accesscontrol.PermissionAccountManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}})
			} // The mutation requires an explicit management grant.
			actor := actorWithGrants(t, workspace, grants...)
			router := chi.NewRouter()
			router.Use(controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}))
			registerProxyRoutes(router, api.NewRegistryProxy(registry.URL, "server-license"), &stubStore{accountID: actor.AccountID})
			request := requestWithActor(t, "POST", "/account/engine/upgrade", actor)
			request.Body = http.NoBody
			request.Header.Set("Authorization", "Bearer personal-credential")
			request.Header.Set("Idempotency-Key", "upgrade-key")
			result := httptest.NewRecorder()
			router.ServeHTTP(result, request)
			expected := 403
			if manage {
				expected = 202
			} // Only authorized callers reach Registry.
			if result.Code != expected || calls != map[bool]int{false: 0, true: 1}[manage] {
				t.Fatalf("status=%d downstream=%d body=%s", result.Code, calls, result.Body.String())
			}
		})
	}
}

// TestHostedEngineAccessReportsServingVersion proves version and management capability are local and secret-free.
func TestHostedEngineAccessReportsServingVersion(t *testing.T) {
	actor := ownerTestActor(t)
	response := httptest.NewRecorder()
	api.HostedEngineAccessHandler("v-test").ServeHTTP(response, requestWithActor(t, "GET", "/account/engine/access", actor))
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	} // Validate the actual browser response.
	if response.Code != 200 || payload["running_version"] != "v-test" || payload["can_manage"] != true || strings.Contains(response.Body.String(), "license") {
		t.Fatal("invalid local access response")
	}
}
