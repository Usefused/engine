package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		configs := &workflowConfigTestStore{state: &store.ConfigState{LatestResourceID: &s.app.AppID, DesiredState: json.RawMessage(`{"name":"private-source","services":{}}`)}}
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

// TestWorkflowSourcePinsPreserveExactVersion keeps a saved service bound to its immutable version.
func TestWorkflowSourcePinsPreserveExactVersion(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	selections, err := json.Marshal([]map[string]any{{"service_id": serviceID, "service_version_id": versionID}})
	// A real immutable selection is required for the source lookup.
	if err != nil {
		t.Fatal(err)
	}
	s := &workflowSourcePinStore{resolved: map[string]uuid.UUID{"Display name": serviceID}}
	source := json.RawMessage(`{"services":{"Display name":{"version":"1.0.0","operations":["read"]}}}`)
	pins, err := workflowSourcePins(context.Background(), s, &store.App{Selections: selections}, source)
	// The editor must receive the exact version after one local identity lookup.
	if err != nil || len(pins) != 1 || s.calls != 1 || pins[0].ServiceID != serviceID || pins[0].ServiceVersionID != versionID {
		t.Fatalf("pin resolution = %#v, calls=%d, error=%v", pins, s.calls, err)
	}
}
