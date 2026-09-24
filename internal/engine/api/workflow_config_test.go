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
	"github.com/Usefused/engine/internal/engine/unified"
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

type workflowSourcePinStore struct {
	store.Store
	resolved map[string]uuid.UUID
	keys     []string
	calls    int
}

// ResolveWorkspaceServiceIDsByKeys captures the bounded identity batch without providing any Registry fallback.
func (s *workflowSourcePinStore) ResolveWorkspaceServiceIDsByKeys(_ context.Context, keys []string) (map[string]uuid.UUID, error) {
	s.calls++
	s.keys = keys
	return s.resolved, nil
}

// TestWorkflowSourcePinsPreserveExactVersion covers the saved display-name/authored-alias mismatch found in the live successor flow.
func TestWorkflowSourcePinsPreserveExactVersion(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	selections, err := json.Marshal([]map[string]any{{"service_id": serviceID, "service_version_id": versionID}})
	// The fixture must carry real immutable identity before testing alias recovery.
	if err != nil {
		t.Fatal(err)
	}
	s := &workflowSourcePinStore{resolved: map[string]uuid.UUID{"Display name": serviceID, "@provider/service": serviceID}}
	source := json.RawMessage(`{"services":{"Display name":{"version":"1.0.0","operations":["read"]}},"unified_operations":{"read":{"bindings":{"step":{"service":"@provider/service","operation":"read"}}}}}`)
	app := workflowSourcePinApp(t, selections, serviceID, versionID)
	pins, err := workflowSourcePins(context.Background(), s, app, source)
	// Both aliases must share the app's exact version after one local query.
	if err != nil || len(pins) != 2 || s.calls != 1 || len(s.keys) != 2 {
		t.Fatalf("pin resolution = %#v, calls=%d, error=%v", pins, s.calls, err)
	}
	for _, pin := range pins {
		// Workspace version labels or defaults cannot replace immutable snapshot IDs.
		if pin.ServiceID != serviceID || pin.ServiceVersionID != versionID {
			t.Fatalf("unexpected pin: %#v", pin)
		}
	}
	// A reused alias must not grant a new provider identity to the successor.
	s.resolved["@provider/service"] = uuid.New()
	if _, err := workflowSourcePins(context.Background(), s, app, source); err == nil {
		t.Fatal("accepted alias outside the exact app scope")
	}
	// Missing or ambiguous aliases must not fall back to a similarly named provider.
	delete(s.resolved, "@provider/service")
	if _, err := workflowSourcePins(context.Background(), s, app, source); err == nil {
		t.Fatal("accepted unresolved alias")
	}
}

// workflowSourcePinApp retains real private executable identities alongside the saved authoring fixture.
func workflowSourcePinApp(t *testing.T, selections json.RawMessage, serviceID, versionID uuid.UUID) *store.App {
	t.Helper()
	program, err := unified.CompileWithTargets(map[string]any{}, unified.DefaultLimits(), []string{"step"})
	// Fixture expressions must be valid before their stored identities are exercised.
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := unified.EncodeDefinitions([]unified.OperationDefinition{{Name: "read", InputSchema: []byte(`{"type":"object"}`), Bindings: []unified.BindingDefinition{{PublicTarget: "step", ServiceTarget: "@provider/service", ServiceID: serviceID, ServiceVersionID: versionID, EndpointID: uuid.New(), OperationID: "read", Input: program}}}}, unified.DefaultLimits())
	// Only canonical executable bytes model a real immutable app.
	if err != nil {
		t.Fatal(err)
	}
	return &store.App{Selections: selections, UnifiedDefinitions: definitions}
}

// TestWorkflowSourceGraphPinsRejectSwappedScopedAliases prevents existing provider identities from being interchanged within one app.
func TestWorkflowSourceGraphPinsRejectSwappedScopedAliases(t *testing.T) {
	first, second, version := uuid.New(), uuid.New(), uuid.New()
	app := workflowSourcePinApp(t, nil, first, version)
	doc := sdkConfigDocument{UnifiedOperations: map[string]sdkUnifiedOperationDoc{"read": {}}}
	// An alias pointing at another allowed provider still changes the original graph's meaning.
	if err := validateWorkflowSourceGraphPins(app, doc, []workflowServicePin{{Key: "@provider/service", ServiceID: second, ServiceVersionID: version}}); err == nil {
		t.Fatal("accepted swapped provider alias")
	}
}

// TestWorkflowVisibilityRequiresCatalogueManagement keeps publication authority out of ordinary discovery grants.
func TestWorkflowVisibilityRequiresCatalogueManagement(t *testing.T) {
	body := []byte(`{"query":"mutation { setWorkflowVisibility(id: \"test\", public: true) { id } }"}`)
	reader := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead)
	// A visible workflow is not authority to publish its private authoring.
	if _, err := authorizeRegistryGraphQLOperation(accesscontrol.ContextWithActor(context.Background(), reader), body); err == nil {
		t.Fatal("reader changed visibility")
	}
	manager := registryPolicyActor(t, accesscontrol.PermissionCatalogueManage)
	// The Registry independently checks ownership after Engine admits catalogue managers.
	if _, err := authorizeRegistryGraphQLOperation(accesscontrol.ContextWithActor(context.Background(), manager), body); err != nil {
		t.Fatal(err)
	}
}
