package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
)

func TestMCPConfigPlanResponseIncludesExactApplyPermissions(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID: serviceID, ServiceName: "okta", Version: "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-07-01"}},
		},
	}
	configStore := &mockConfigStore{}
	registryClient := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|2026-07-01": {
			ServiceID: serviceID, Version: "2026-07-01", ServiceVersionID: serviceVersionID, Revision: 1,
		},
	}}
	router := newControlTestRouter(s.accountID)
	router.Post("/mcp-config/plan", MCPConfigPlanHandler(configStore, s, registryClient))
	body := []byte(`{
		"source_hash":"abc",
		"owner_team":"platform","config_key":"mcp:security:1.0.0",
		"config":{"apiVersion":"fused/v1","kind":"mcp","name":"security","version":"1.0.0","bucket":"default","services":{"okta":{"version":"2026-07-01","select_all":true}}}
	}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mcp-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		RequiredPermissions []requiredPermissionResponse `json:"required_permissions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !hasRequiredPermission(payload.RequiredPermissions, "service.consume", "service", serviceID) {
		t.Fatalf("required permissions = %#v", payload.RequiredPermissions)
	}
}

func TestConfigPlanActionsRefreshesRequiredPermissionsInResponse(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	configStore := &mockConfigStore{plan: &store.ConfigPlan{ID: uuid.New(), Status: store.ConfigPlanStatusPending}}
	router := newControlTestRouter(accountID)
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, _ := accesscontrol.ActorFromContext(r.Context())
			requirements := []accesscontrol.Requirement{
				{Permission: accesscontrol.PermissionWorkspaceUpdate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}},
				{Permission: accesscontrol.PermissionServiceManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
			}
			next.ServeHTTP(w, r.WithContext(accesscontrol.ContextWithRequiredPermissions(r.Context(), requirements)))
		})
	})
	router.Patch("/config/plans/{planId}/actions", ConfigPlanActionsHandler(configStore, &workspaceTestStore{accountID: accountID}))
	body := []byte(`{"actions":[{"type":"remove_service","service_id":"` + serviceID.String() + `","service_name":"Stripe"}]}`)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/config/plans/"+configStore.plan.ID.String()+"/actions", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		RequiredPermissions []requiredPermissionResponse `json:"required_permissions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !hasRequiredPermission(payload.RequiredPermissions, "service.manage", "service", serviceID) {
		t.Fatalf("required permissions = %#v", payload.RequiredPermissions)
	}
	if string(configStore.plan.RequiredPermissions) == "" {
		t.Fatal("updated plan did not persist required permissions")
	}
}
