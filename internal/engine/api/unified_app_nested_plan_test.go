package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type nestedPlanStore struct {
	*executionHTTPTestStore
	bindings []models.UnifiedAppBinding
}

// ResolveUnifiedAppBindings supplies the current exact child identity for plan/apply revalidation.
func (fixture *nestedPlanStore) ResolveUnifiedAppBindings(context.Context, uuid.UUID, map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error) {
	return fixture.bindings, nil
}

// ReadUnifiedAppBindings is unused by control-plane tests and cannot grant runtime authority here.
func (*nestedPlanStore) ReadUnifiedAppBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.UnifiedAppBinding, error) {
	return nil, store.ErrAppNotFound
}

// TestUnifiedAppNestedPlanPinsPermissionsAndRechecksApply exercises reference-only planning through the real HTTP path.
func TestUnifiedAppNestedPlanPinsPermissionsAndRechecksApply(t *testing.T) {
	child := models.UnifiedAppBinding{Alias: "child", Name: "Child", Version: "1.0.0", AppID: uuid.New(), AppFamilyID: uuid.New(), SourceHash: "sha256:child", BundleDigest: store.UnifiedAppBundleDigest([]byte("child"))}
	workspace := &nestedPlanStore{executionHTTPTestStore: &executionHTTPTestStore{&workspaceTestStore{accountID: uuid.New(), workspaceID: uuid.New()}}, bindings: []models.UnifiedAppBinding{child}}
	configs := &mockConfigStore{}
	router := newControlTestRouter(workspace.accountID)
	router.Post("/unified-app-config/plan", UnifiedAppConfigPlanHandler(configs, workspace, &mockRegistryClient{}))
	router.Post("/unified-app-config/apply", UnifiedAppConfigApplyHandler(configs, workspace, &mockRegistryClient{}))
	body, _ := json.Marshal(map[string]any{"source_hash": "sha256:parent", "config_key": "unified_app:Parent:1.0.0", "owner_team": "platform", "config": map[string]any{
		"apiVersion": "fused/v1", "kind": "unified_app", "name": "Parent", "version": "1.0.0", "bucket": "default", "bundle_digest": store.UnifiedAppBundleDigest([]byte("parent")),
		"unified_apps": map[string]any{"child": map[string]any{"name": "Child", "version": "1.0.0"}},
	}})
	request := httptest.NewRequest(http.MethodPost, "/unified-app-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// Hosted-only scope must reach a durable plan, including the child's use requirement.
	if response.Code != http.StatusOK || configs.createdPlan == nil {
		t.Fatalf("plan=%d %s", response.Code, response.Body.String())
	}
	var payload appResolvedPayload
	// The child must remain pinned by runtime identity rather than only by its authored name.
	if json.Unmarshal(configs.createdPlan.ResolvedPayload, &payload) != nil || len(payload.UnifiedApps) != 1 || payload.UnifiedApps[0].AppID != child.AppID {
		t.Fatal("child pin missing")
	}
	// Actor and owner preflight must receive the same app.use permission required by SDK/MCP consumers.
	if !strings.Contains(string(configs.createdPlan.RequiredPermissions), "app.unified_app.use") || !strings.Contains(string(configs.createdPlan.RequiredPermissions), child.AppFamilyID.String()) {
		t.Fatalf("permissions=%s", configs.createdPlan.RequiredPermissions)
	}
	workspace.bindings[0].AppID = uuid.New()
	body, _ = json.Marshal(map[string]any{"plan_id": configs.plan.ID, "source_hash": "sha256:parent", "skip_token": true})
	request = httptest.NewRequest(http.MethodPost, "/unified-app-config/apply", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// A dependency changed since review must never be silently substituted during apply.
	if response.Code != http.StatusConflict || configs.artifactApply != nil {
		t.Fatalf("apply=%d %s", response.Code, response.Body.String())
	}
}
