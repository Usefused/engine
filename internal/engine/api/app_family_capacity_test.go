package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// TestCheckSDKFamilyCapacityReportsEntitlementError keeps SDK quota failures
// actionable and distinct from authorization failures in CLI output.
func TestCheckSDKFamilyCapacityReportsEntitlementError(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxSDKFamilies: models.IntPtr(0)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)

	err := checkSDKFamilyCapacity(context.Background(), &workspaceTestStore{accountID: accountID}, trace.SpanFromContext(context.Background()), accountID, "replacement", true)
	assertSDKFamilyLimitError(t, err)
}

// TestCheckAPIFamilyCapacityReportsIndependentEntitlement proves package-free apps do not consume or report the SDK ceiling.
func TestCheckAPIFamilyCapacityReportsIndependentEntitlement(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxAPIFamilies: models.IntPtr(0), MaxSDKFamilies: models.IntPtr(10)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)

	err := checkSDKFamilyCapacity(context.Background(), &workspaceTestStore{accountID: accountID}, trace.SpanFromContext(context.Background()), accountID, "replacement", false)
	assertAPIFamilyLimitError(t, err)
}

// TestAPIApplyPersistenceErrorPreservesQuotaIdentity covers the transaction-time race after plan admission.
func TestAPIApplyPersistenceErrorPreservesQuotaIdentity(t *testing.T) {
	err := appApplyPersistenceError(context.Background(), store.ErrAPIFamilyLimitExceeded, uuid.New())
	httpErr, ok := err.(workspaceConfigHTTPError)
	// The commit-time guard must return the same stable API code as the earlier read-only capacity check.
	if !ok || httpErr.code != "api_family_limit_exceeded" || httpErr.category != "entitlement" {
		t.Fatalf("unexpected API persistence error: %#v", err)
	}
}

// TestSDKPlanRejectsFamilyQuotaBeforeResolution proves capacity is a plan blocker rather than a late generation failure.
func TestSDKPlanRejectsFamilyQuotaBeforeResolution(t *testing.T) {
	accountID, familyID := uuid.New(), uuid.New()
	s := appFamilyQuotaPlanStore(accountID, familyID, store.AppKindSDK, "existing-sdk")
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxSDKFamilies: models.IntPtr(1)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	router := newControlTestRouter(accountID)
	router.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, &mockRegistryClient{}))
	body := []byte(`{"source_hash":"sha256:test","owner_team":"platform","config_key":"sdk:new-sdk:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"new-sdk","version":"1.0.0","language":"typescript","bucket":"default","services":{"unresolved":{"version":"v1","select_all":true}}}}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	router.ServeHTTP(response, request)
	assertAppPlanFamilyQuotaResponse(t, response, "sdk_family_limit_exceeded")
}

// TestAPIPlanRejectsOnlyAPIQuota proves generate false selects the independent direct-API ceiling during plan admission.
func TestAPIPlanRejectsOnlyAPIQuota(t *testing.T) {
	accountID, familyID := uuid.New(), uuid.New()
	s := appFamilyQuotaPlanStore(accountID, familyID, store.AppKindSDK, "existing-api")
	for appID, app := range s.apps {
		app.SDKGenerationStatus = models.SDKGenerationStatusSkipped
		s.apps[appID] = app
	}
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxAPIFamilies: models.IntPtr(1), MaxSDKFamilies: models.IntPtr(10)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	router := newControlTestRouter(accountID)
	router.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, &mockRegistryClient{}))
	body := []byte(`{"source_hash":"sha256:test","owner_team":"platform","config_key":"sdk:new-api:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"new-api","version":"1.0.0","language":"typescript","bucket":"default","generate":false,"services":{"unresolved":{"version":"v1","select_all":true}}}}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	router.ServeHTTP(response, request)
	assertAppPlanFamilyQuotaResponse(t, response, "api_family_limit_exceeded")
}

// TestMCPPlanRejectsFamilyQuotaBeforeResolution proves MCP reports the same plan-time capacity contract.
func TestMCPPlanRejectsFamilyQuotaBeforeResolution(t *testing.T) {
	accountID, familyID := uuid.New(), uuid.New()
	s := appFamilyQuotaPlanStore(accountID, familyID, store.AppKindMCP, "existing-mcp")
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(1)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	router := newControlTestRouter(accountID)
	router.Post("/mcp-config/plan", MCPConfigPlanHandler(&mockConfigStore{}, s, &mockRegistryClient{}))
	body := []byte(`{"source_hash":"sha256:test","owner_team":"platform","config_key":"mcp:new-mcp:1.0.0","config":{"apiVersion":"fused/v1","kind":"mcp","name":"new-mcp","version":"1.0.0","description":"Exercise plan admission.","bucket":"default","services":{"unresolved":{"version":"v1","select_all":true}}}}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mcp-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	router.ServeHTTP(response, request)
	assertAppPlanFamilyQuotaResponse(t, response, "mcp_family_limit_exceeded")
}

// TestSDKApplyRejectsFamilyQuotaBeforeRegistry proves a post-plan capacity race is still pre-mutation admission.
func TestSDKApplyRejectsFamilyQuotaBeforeRegistry(t *testing.T) {
	fixture := newSDKGenerationAmbiguityFixture(t)
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxSDKFamilies: models.IntPtr(0)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	proxy := &recordingForwarder{}

	_, err := executeSDKConfigApply(context.Background(), fixture.configStore, fixture.engineStore, proxy, fixture.registry, fixture.call)
	// The limit must stop before any Registry request can create an externally ambiguous outcome.
	if err == nil {
		t.Fatal("expected SDK apply capacity rejection")
	}
	httpErr, ok := err.(workspaceConfigHTTPError)
	// Admission metadata proves the caller may repair capacity and retry without inspecting Registry generation state.
	if !ok || httpErr.code != "sdk_family_limit_exceeded" || httpErr.phase != "apply_admission" || httpErr.commitState != "not_committed" {
		t.Fatalf("unexpected SDK apply quota error: %#v", err)
	}
	// No proxy call means the Engine has not crossed the external package-generation boundary.
	if len(proxy.forwardMethods) != 0 {
		t.Fatalf("quota rejection reached Registry: methods=%v", proxy.forwardMethods)
	}
	// The early check also avoids creating a recovery lease for work that never began.
	if fixture.configStore.applyLeaseID != uuid.Nil {
		t.Fatalf("quota rejection reserved apply lease %s", fixture.configStore.applyLeaseID)
	}
}

// appFamilyQuotaPlanStore creates one invokable family so a second app exercises plan admission at the configured ceiling.
func appFamilyQuotaPlanStore(accountID, familyID uuid.UUID, kind store.AppKind, canonicalName string) *workspaceTestStore {
	return &workspaceTestStore{
		accountID: accountID,
		appFamilies: map[string]store.AppFamily{
			accountID.String() + "\x00" + kind.String() + "\x00" + canonicalName: {
				AppFamilyID: familyID, AccountID: accountID, Kind: kind, CanonicalName: canonicalName,
			},
		},
		apps: map[uuid.UUID]store.App{uuid.New(): {AppFamilyID: familyID, Status: store.AppStatusActive}},
	}
}

// assertAppPlanFamilyQuotaResponse verifies stable plan classification without depending on downstream service resolution.
func assertAppPlanFamilyQuotaResponse(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	// Capacity is a reviewed entitlement failure, not an internal planner or permission error.
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	var payload workspaceConfigErrorResponse
	// The structured envelope is the CLI's authority for safe retry and remediation behavior.
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode quota response: %v", err)
	}
	// A plan rejection cannot have committed app or Registry state.
	if payload.Error.Code != code || payload.Error.Phase != "plan_admission" || payload.Error.CommitState != "not_committed" {
		t.Fatalf("unexpected plan quota response: %#v", payload.Error)
	}
}

// assertSDKFamilyLimitError verifies both the internal classification and the public wire envelope.
func assertSDKFamilyLimitError(t *testing.T, err error) {
	t.Helper()
	assertSDKFamilyLimitType(t, err)
	assertSDKFamilyLimitResponse(t, err)
}

// assertSDKFamilyLimitType verifies the admission layer returns reviewed entitlement metadata.
func assertSDKFamilyLimitType(t *testing.T, err error) {
	t.Helper()
	// A plan ceiling remains HTTP 403 while its stable code identifies entitlement rather than RBAC.
	if err == nil {
		t.Fatal("expected SDK family limit error")
	}
	httpErr, ok := err.(workspaceConfigHTTPError)
	// The public envelope must include bounded quota detail and a concrete recovery path.
	if !ok || httpErr.status != http.StatusForbidden || httpErr.code != "sdk_family_limit_exceeded" || httpErr.category != "entitlement" || httpErr.message != "This workspace has reached its SDK limit (0 of 0)." || httpErr.remediation != "Deactivate all active or deprecated versions of an unused SDK, or upgrade the workspace plan, then retry." {
		t.Fatalf("unexpected SDK family limit error: %#v", err)
	}
}

// assertSDKFamilyLimitResponse verifies HTTP serialization cannot relabel quota as authorization denial.
func assertSDKFamilyLimitResponse(t *testing.T, err error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	writeSDKConfigError(recorder, err)
	var response workspaceConfigErrorResponse
	// The actual wire serializer must preserve quota identity instead of deriving permission_denied from HTTP 403.
	if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &response); decodeErr != nil {
		t.Fatalf("decode SDK family limit response: %v", decodeErr)
	}
	// CLI consumers depend on this explicit code and remediation to avoid misleading RBAC guidance.
	if response.Error.Code != "sdk_family_limit_exceeded" || response.Error.Category != "entitlement" || response.Error.Message != "This workspace has reached its SDK limit (0 of 0)." || response.Error.Remediation != "Deactivate all active or deprecated versions of an unused SDK, or upgrade the workspace plan, then retry." {
		t.Fatalf("unexpected SDK family limit response: %#v", response.Error)
	}
}

// assertAPIFamilyLimitError verifies direct API capacity keeps its own stable public identity.
func assertAPIFamilyLimitError(t *testing.T, err error) {
	t.Helper()
	httpErr, ok := err.(workspaceConfigHTTPError)
	// API quota must remain distinct from both generated SDK capacity and authorization denial.
	if !ok || httpErr.status != http.StatusForbidden || httpErr.code != "api_family_limit_exceeded" || httpErr.category != "entitlement" || httpErr.message != "This workspace has reached its API limit (0 of 0)." {
		t.Fatalf("unexpected API family limit error: %#v", err)
	}
}
