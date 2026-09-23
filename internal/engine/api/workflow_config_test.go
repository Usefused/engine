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
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type workflowConfigTestStore struct {
	store.ConfigRepository
	state *store.ConfigState
	reads int
}

// GetConfigState records whether authorization happened before the first private-source read.
func (s *workflowConfigTestStore) GetConfigState(context.Context, string) (*store.ConfigState, error) {
	s.reads++
	return s.state, nil
}

// TestWorkflowSourceRequiresEditAuthority proves ordinary app readers cannot fetch private executable mappings.
func TestWorkflowSourceRequiresEditAuthority(t *testing.T) {
	for _, permission := range []accesscontrol.Permission{accesscontrol.PermissionAppSDKRead, accesscontrol.PermissionAppSDKManage} {
		s, _ := newAppOpenAPIFixture(t)
		configs := &workflowConfigTestStore{state: &store.ConfigState{LatestResourceID: &s.app.AppID, DesiredState: json.RawMessage(`{"name":"private-source","unified_operations":{}}`)}}
		actor := registryPolicyActor(t, permission)
		actor.AccountID = s.app.AccountID
		router := chi.NewRouter()
		router.Get("/apps/{app_id}/config", AppConfigSourceHandler(s, configs))
		request := httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/config", nil)
		request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		// The failed read path must not reach the private desired-state store at all.
		if permission == accesscontrol.PermissionAppSDKRead {
			if response.Code != http.StatusForbidden || configs.reads != 0 {
				t.Fatalf("private source exposed: %d/%d", response.Code, configs.reads)
			}
			continue
		}
		// An authorized source export must be exact and explicitly noncacheable.
		if response.Code != http.StatusOK || configs.reads != 1 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("authorized source failed: %d %s", response.Code, response.Body.String())
		}
	}
}

// TestWorkflowSourcesCanonicalIdentity ensures reordered provenance is a no-op and changed provenance remains immutable state.
func TestWorkflowSourcesCanonicalIdentity(t *testing.T) {
	first := workflowSource{ID: uuid.NewString(), Version: "1.0.0", Hash: "sha256:" + strings.Repeat("a", 64)}
	second := workflowSource{ID: uuid.NewString(), Version: "1.0.0", Hash: "sha256:" + strings.Repeat("b", 64)}
	left, err := canonicalAppState(sdkConfigDocument{WorkflowSources: []workflowSource{first, second}})
	// Canonicalization must succeed before comparing identity across source orderings.
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalAppState(sdkConfigDocument{WorkflowSources: []workflowSource{second, first}})
	// Source ordering must not create a spurious immutable app version.
	if err != nil || string(left) != string(right) {
		t.Fatalf("source ordering changed app identity: %v", err)
	}
	// Duplicate identities could otherwise hide one requested release behind another.
	if err := validateWorkflowSources([]workflowSource{first, first}); err == nil {
		t.Fatal("duplicate source accepted")
	}
}

// TestWorkflowCataloguePolicies separates read access from explicit publication authority.
func TestWorkflowCataloguePolicies(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	// Catalogue policy must recognize the intended operation without requiring unrelated app authority.
	if _, err := authorizeRegistryGraphQLOperation(ctx, []byte(`{"query":"query { workflows { total } }"}`)); err != nil {
		t.Fatal(err)
	}
	// Catalogue discovery does not grant permission to publish shared executable authoring.
	if _, err := authorizeRegistryGraphQLOperation(ctx, []byte(`{"query":"mutation { publishWorkflow(template: \"{}\", public: false) { id } }"}`)); err == nil {
		t.Fatal("reader can publish")
	}
	actor = registryPolicyActor(t, accesscontrol.PermissionCatalogueManage)
	ctx = accesscontrol.ContextWithActor(context.Background(), actor)
	// Catalogue policy must recognize the intended operation without requiring unrelated app authority.
	if _, err := authorizeRegistryGraphQLOperation(ctx, []byte(`{"query":"mutation { publishWorkflow(template: \"{}\", public: false) { id } }"}`)); err != nil {
		t.Fatal(err)
	}
}
