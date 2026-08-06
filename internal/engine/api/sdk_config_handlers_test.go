package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestValidateSDKServiceSelection_NotActivatedErrorMessage is Task 6's
// clearer-error AC (engine_workspace_registration_plan.md): the old message
// ("service %s is not allowed in this workspace") didn't tell the developer
// what to do about it. The new one names the fix directly.
func TestValidateSDKServiceSelection_NotActivatedErrorMessage(t *testing.T) {
	_, _, err := validateSDKServiceSelection(
		context.Background(),
		nil,
		"stripe",
		sdkConfigServiceDoc{Version: "2026-01-01", Operations: []string{"createCharge"}},
		store.WorkspaceService{},
		false,
		nil,
	)
	if err == nil {
		t.Fatal("expected an error when the service isn't activated")
	}
	want := "service stripe is not activated in this workspace. Run 'fused-cli workspace service add stripe' to activate it."
	if err.Error() != want {
		t.Errorf("expected message %q, got %q", want, err.Error())
	}
}

func TestCanonicalAppStateIgnoresSetOrdering(t *testing.T) {
	first, err := canonicalAppState(sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "mcp", Name: "reader", Version: "1.0.0",
		Services: map[string]sdkConfigServiceDoc{
			"github": {Version: "2026-07-01", Operations: []string{"getUser", "listRepos", "getUser"}, Connect: &sdkAppConnectDoc{Scopes: []string{"repo", "read:user"}}},
		},
	})
	if err != nil {
		t.Fatalf("canonicalAppState(first): %v", err)
	}
	second, err := canonicalAppState(sdkConfigDocument{
		APIVersion: " fused/v1 ", Kind: "mcp", Name: " reader ", Version: "1.0.0",
		Services: map[string]sdkConfigServiceDoc{
			"github": {Version: " 2026-07-01 ", Operations: []string{"listRepos", "getUser"}, Connect: &sdkAppConnectDoc{Scopes: []string{"read:user", "repo"}}},
		},
	})
	if err != nil {
		t.Fatalf("canonicalAppState(second): %v", err)
	}
	if !sameCanonicalAppState(first, second) {
		t.Fatalf("equivalent app configs must share canonical state: %s != %s", first, second)
	}
}

func TestAppResolvedPayloadHasNoTarget(t *testing.T) {
	payload, err := json.Marshal(appResolvedPayload{})
	if err != nil {
		t.Fatalf("marshal app payload: %v", err)
	}
	if strings.Contains(string(payload), "target") {
		t.Fatalf("MCP resolved payload must not carry a target: %s", payload)
	}
}

func TestPlannedAppIDUsesResolvedIdentityWithoutAppliedState(t *testing.T) {
	id := uuid.New()
	plan := &store.ConfigPlan{ResolvedPayload: json.RawMessage(`{"app_id":"` + id.String() + `"}`)}
	got := plannedAppID(plan, nil)
	if got == nil || *got != id {
		t.Fatalf("planned app id = %v, want %s", got, id)
	}
}

func TestAppPermissionStateTreatsResolvedIdentityAsExisting(t *testing.T) {
	id := uuid.New()
	state := appPermissionState(nil, id)
	if state == nil || state.LatestResourceID == nil || *state.LatestResourceID != id {
		t.Fatalf("permission state did not retain restored identity: %+v", state)
	}
}

func TestSDKNoOpSummaryHasNoUpdateOrOperationAdditions(t *testing.T) {
	service := sdkServiceSummary("Jira", sdkConfigServiceDoc{Version: "1.0.0", Operations: []string{"createIssue", "getCurrentUser"}}, []string{"getCurrentUser", "createIssue"})
	summary := sdkPlanSummary(false, false, []map[string]any{service})
	if summary["create_sdk"] != false || summary["update_sdk"] != false {
		t.Fatalf("unexpected no-op summary: %+v", summary)
	}
	if added := service["operations_added"].([]string); len(added) != 0 {
		t.Fatalf("unchanged operations were reported as additions: %+v", service)
	}
}

func TestExecuteSDKConfigApplyNoopDoesNotCallRegistryOrRotateToken(t *testing.T) {
	appID, planID, accountID := uuid.New(), uuid.New(), uuid.New()
	payload, err := json.Marshal(appResolvedPayload{AppID: appID, Noop: true, BucketID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	state := &store.ConfigState{ConfigKey: "sdk:jira:1.0.0", ConfigType: store.ConfigTypeSDK,
		DesiredState: json.RawMessage(`{"name":"jira"}`), ManagedResources: json.RawMessage(`{"keep":true}`), LatestResourceID: &appID}
	configStore := &mockConfigStore{state: state, plan: &store.ConfigPlan{
		ID: planID, Revision: 1, ConfigKey: state.ConfigKey, ConfigType: store.ConfigTypeSDK,
		SourceHash: "same", Status: store.ConfigPlanStatusPending, DesiredState: state.DesiredState, ResolvedPayload: payload,
	}}
	proxy := &recordingForwarder{}
	s := &workspaceTestStore{mockScopes: map[uuid.UUID]*store.AppRuntime{
		appID: {AppID: appID, AccountID: accountID, Version: "1.0.0", ConfigKey: state.ConfigKey},
	}}
	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, &mockRegistryClient{}, sdkApplyCall{
		accountID: accountID, planID: planID, planRevision: 1, sourceHash: "same",
	})
	if err != nil {
		t.Fatalf("no-op apply: %v", err)
	}
	if proxy.forwardCalled || proxy.forwardAndInspectCalled {
		t.Fatal("no-op apply contacted Registry generation")
	}
	if configStore.artifactApply != nil || result.ExecutionToken != "" {
		t.Fatalf("no-op apply changed runtime scope or token: apply=%+v token=%q", configStore.artifactApply, result.ExecutionToken)
	}
	if !configStore.markApplied || result.AppID != appID || result.Status != models.SDKGenerationStatusComplete {
		t.Fatalf("no-op apply did not finalize the plan: result=%+v applied=%v", result, configStore.markApplied)
	}
}

func resolvedDefaultBucketPayload(t *testing.T, request GenerateSDKRequest) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(resolvedSDKPayload(request, workspaceTestBucketID("default"), uuid.Nil, false))
	if err != nil {
		t.Fatalf("marshal resolved SDK payload: %v", err)
	}
	return payload
}

func TestValidateAppBucketReadinessReportsEveryMissingOAuthService(t *testing.T) {
	bucketID := uuid.New()
	first, second := uuid.New(), uuid.New()
	s := &workspaceTestStore{
		bucketsByName: map[string]*store.Bucket{
			"production": {ID: bucketID},
		},
	}
	err := validateAppBucketReadiness(context.Background(), s, bucketID, []models.SDKSelection{
		{ServiceID: first, AuthType: "oauth"}, {ServiceID: second, AuthType: "oidc"},
	})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a structured readiness error, got %v", err)
	}
	missing, ok := httpErr.details["missing"].([]string)
	if !ok || !slices.Contains(missing, first.String()+" (oauth)") || !slices.Contains(missing, second.String()+" (oidc)") {
		t.Fatalf("expected structured details containing both services, got %#v", httpErr.details)
	}
}

// TestValidateWebhookAttachmentCoverage_NoAttachmentSkipsLookup proves a
// config with no webhook_attachment never touches the store -- there is
// nothing to cover, and validateWebhookAttachmentRequired already guarantees
// no service selects webhooks in this case.
func TestValidateWebhookAttachmentCoverage_NoAttachmentSkipsLookup(t *testing.T) {
	configStore := &mockConfigStore{err: errors.New("must not be called")}
	err := validateWebhookAttachmentCoverage(context.Background(), configStore, sdkConfigDocument{
		Services: map[string]sdkConfigServiceDoc{"github": {Webhooks: []string{"push"}}},
	})
	if err != nil {
		t.Fatalf("expected no lookup and no error for an empty webhook_attachment, got %v", err)
	}
}

// TestValidateWebhookAttachmentCoverage_RejectsMissingAttachment mirrors
// resolveSDKBucketID's "bucket not found" behavior for the referenced
// kind: webhook artifact -- same pattern (SDK/MCP references another entity
// by name, that entity must already exist), same clear rejection.
func TestValidateWebhookAttachmentCoverage_RejectsMissingAttachment(t *testing.T) {
	configStore := &mockConfigStore{} // no state -- artifact was never applied
	err := validateWebhookAttachmentCoverage(context.Background(), configStore, sdkConfigDocument{
		WebhookAttachment: "team-x-webhooks",
		Services:          map[string]sdkConfigServiceDoc{"github": {Webhooks: []string{"push"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "webhook attachment not found: team-x-webhooks") {
		t.Fatalf("expected a clear missing-attachment error, got %v", err)
	}
}

// TestValidateWebhookAttachmentCoverage_RejectsUnregisteredService is the
// core fix: a service selecting webhooks whose attached kind: webhook
// artifact never registered it used to fail silently (no error at plan or
// apply, the events just never arrive) -- see plans/plan-webhook-kind.md's
// "known gap" note.
func TestValidateWebhookAttachmentCoverage_RejectsUnregisteredService(t *testing.T) {
	configStore := &mockConfigStore{state: &store.ConfigState{
		DesiredState: []byte(`{"name":"team-x-webhooks","services":{"jira":{"secret":"${bucket.secret.jira_signing}"}}}`),
	}}
	err := validateWebhookAttachmentCoverage(context.Background(), configStore, sdkConfigDocument{
		WebhookAttachment: "team-x-webhooks",
		Services:          map[string]sdkConfigServiceDoc{"github": {Webhooks: []string{"push"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "service github") || !strings.Contains(err.Error(), `"team-x-webhooks"`) {
		t.Fatalf("expected a clear unregistered-service error, got %v", err)
	}
}

// TestValidateWebhookAttachmentCoverage_AcceptsRegisteredService proves the
// happy path: a service present in the attached artifact's services map
// passes, whether it selected explicit event names or webhooks_select_all.
func TestValidateWebhookAttachmentCoverage_AcceptsRegisteredService(t *testing.T) {
	configStore := &mockConfigStore{state: &store.ConfigState{
		DesiredState: []byte(`{"name":"team-x-webhooks","services":{"github":{"secret":""},"jira":{}}}`),
	}}
	err := validateWebhookAttachmentCoverage(context.Background(), configStore, sdkConfigDocument{
		WebhookAttachment: "team-x-webhooks",
		Services: map[string]sdkConfigServiceDoc{
			"github": {Webhooks: []string{"push"}},
			"jira":   {WebhooksSelectAll: true},
		},
	})
	if err != nil {
		t.Fatalf("expected registered services to pass, got %v", err)
	}
}

// TestValidateWebhookAttachmentCoverage_IgnoresServicesWithoutWebhooks
// proves a service that selects only operations (no webhooks) is never
// checked against the attached artifact's coverage -- it has no delivery
// dependency on it at all.
func TestValidateWebhookAttachmentCoverage_IgnoresServicesWithoutWebhooks(t *testing.T) {
	configStore := &mockConfigStore{state: &store.ConfigState{
		DesiredState: []byte(`{"name":"team-x-webhooks","services":{}}`),
	}}
	err := validateWebhookAttachmentCoverage(context.Background(), configStore, sdkConfigDocument{
		WebhookAttachment: "team-x-webhooks",
		Services:          map[string]sdkConfigServiceDoc{"github": {Operations: []string{"listRepos"}}},
	})
	if err != nil {
		t.Fatalf("expected an operations-only service to skip coverage checking, got %v", err)
	}
}

// TestValidateWebhookAttachmentCoverage_PropagatesLookupError proves a store
// failure fails closed rather than silently treating it as "not found" or
// "covered".
func TestValidateWebhookAttachmentCoverage_PropagatesLookupError(t *testing.T) {
	configStore := &mockConfigStore{err: errors.New("boom")}
	err := validateWebhookAttachmentCoverage(context.Background(), configStore, sdkConfigDocument{
		WebhookAttachment: "team-x-webhooks",
		Services:          map[string]sdkConfigServiceDoc{"github": {Webhooks: []string{"push"}}},
	})
	if err == nil {
		t.Fatal("expected the config state lookup error to propagate")
	}
}

func TestValidateSDKConfigDocumentRequiresBucket(t *testing.T) {
	err := validateSDKConfigDocument(sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "sdk", Name: "reader", Version: "1.0.0", Language: "typescript",
		Services: map[string]sdkConfigServiceDoc{"github": {Version: "1.0", Operations: []string{"listRepos"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one bucket") {
		t.Fatalf("expected missing bucket validation error, got %v", err)
	}
}

func TestSDKConfigPlanHandler_UsesOperations(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   serviceID,
			ServiceName: "okta",
			Version:     "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-07-01"}},
		},
	}
	configStore := &mockConfigStore{}
	registryClient := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|2026-07-01": {ServiceID: serviceID, Version: "2026-07-01", ServiceVersionID: serviceVersionID, Revision: 1},
	}}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
            "bucket": "default",
            "services": {
				"okta": {
					"version": "2026-07-01",
					"operations": ["listLogEvents"]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resolved GenerateSDKRequest
	if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil {
		t.Fatalf("decode resolved payload: %v", err)
	}
	if len(resolved.Selections) != 1 {
		t.Fatalf("expected one selection, got %#v", resolved.Selections)
	}
	if got := resolved.Selections[0].OperationNames; len(got) != 1 || got[0] != "listLogEvents" {
		t.Fatalf("expected operationId to resolve into operation names, got %#v", got)
	}
	if resolved.Selections[0].ServiceID != serviceID {
		t.Fatalf("expected service %s, got %s", serviceID, resolved.Selections[0].ServiceID)
	}
	var response struct {
		RequiredPermissions []requiredPermissionResponse `json:"required_permissions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode plan response: %v", err)
	}
	if !hasRequiredPermission(response.RequiredPermissions, "service.consume", "service", serviceID) {
		t.Fatalf("expected service.consume preview, got %#v", response.RequiredPermissions)
	}
	if configStore.createdPlan == nil || configStore.createdPlan.OwnerSubjectID != nil || configStore.createdPlan.OwnerTeamID == nil || *configStore.createdPlan.OwnerTeamID != testAppOwnerTeamID {
		t.Fatalf("persisted team slug owner = %#v", configStore.createdPlan)
	}
	var persisted []requiredPermissionResponse
	if err := json.Unmarshal(configStore.createdPlan.RequiredPermissions, &persisted); err != nil {
		t.Fatalf("decode persisted permissions: %v", err)
	}
	if !hasRequiredPermission(persisted, "service.consume", "service", serviceID) {
		t.Fatalf("expected persisted service.consume, got %#v", persisted)
	}
}

// TestSDKConfigPlanHandler_RejectsUnregisteredWebhookAttachment is the
// end-to-end wiring proof for validateWebhookAttachmentCoverage: a service
// selecting webhooks whose webhook_attachment names an artifact that was
// never applied must fail plan, not silently produce a config that never
// receives deliveries.
func TestSDKConfigPlanHandler_RejectsUnregisteredWebhookAttachment(t *testing.T) {
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
	configStore := &mockConfigStore{} // "webhook:team-x-webhooks" was never applied
	registryClient := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|2026-07-01": {ServiceID: serviceID, Version: "2026-07-01", ServiceVersionID: serviceVersionID, Revision: 1},
	}}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
			"bucket": "default",
			"webhook_attachment": "team-x-webhooks",
			"services": {
				"okta": {
					"version": "2026-07-01",
					"operations": ["listLogEvents"],
					"webhooks": ["log.created"]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "webhook attachment not found: team-x-webhooks") {
		t.Fatalf("expected a clear webhook attachment error, got %s", rr.Body.String())
	}
}

func TestSDKConfigPlanHandler_FailsClosedWithoutBatchVersionResolver(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:         uuid.New(),
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "okta", Version: "1.0"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		},
	}
	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, nil))
	body := []byte(`{"source_hash":"config-hash",
		"owner_team":"platform","config_key":"sdk:security:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"security","version":"1.0.0","language":"typescript","bucket":"default","services":{"okta":{"version":"1.0","operations":["listLogEvents"]}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected fail-closed 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSDKConfigPlanBindsSelectedContractRevision(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:                uuid.New(),
		workspaceServices:        []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "okta", Version: "1.0"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "spec-hash-3"},
	}}
	configStore := &mockConfigStore{}
	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registry))
	body := []byte(`{"source_hash":"config-hash",
		"owner_team":"platform","config_key":"sdk:security:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"security","version":"1.0.0","language":"typescript","bucket":"default","services":{"okta":{"version":"1.0","operations":["listLogEvents"]}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resolved GenerateSDKRequest
	if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil {
		t.Fatal(err)
	}
	if len(resolved.ContractBindings) != 1 || resolved.ContractBindings[0].Revision != 3 || resolved.ContractBindings[0].SourceHash != "spec-hash-3" {
		t.Fatalf("expected immutable contract binding, got %#v", resolved.ContractBindings)
	}
	if resolved.Selections[0].ServiceVersionID != serviceVersionID {
		t.Fatalf("expected selection pinned to service_version_id %s, got %#v", serviceVersionID, resolved.Selections[0])
	}
}

func TestSDKConfigPlanBatchesVersionResolutionForMultipleServices(t *testing.T) {
	oktaID := uuid.New()
	githubID := uuid.New()
	oktaVersionID := uuid.New()
	githubVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: oktaID, ServiceName: "okta", Version: "1.0"},
			{ServiceID: githubID, ServiceName: "github", Version: "2.0"},
		},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			oktaID:   {{ServiceID: oktaID, Version: "1.0"}},
			githubID: {{ServiceID: githubID, Version: "2.0"}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		oktaID.String() + "|1.0":   {ServiceID: oktaID, Version: "1.0", ServiceVersionID: oktaVersionID, Revision: 1, SourceHash: "okta-hash"},
		githubID.String() + "|2.0": {ServiceID: githubID, Version: "2.0", ServiceVersionID: githubVersionID, Revision: 2, SourceHash: "github-hash"},
	}}
	configStore := &mockConfigStore{}
	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registry))
	body := []byte(`{"source_hash":"config-hash",
		"owner_team":"platform","config_key":"sdk:security:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"security","version":"1.0.0","language":"typescript","bucket":"default","services":{"okta":{"version":"1.0","operations":["listLogEvents"]},"github":{"version":"2.0","operations":["listRepos"]}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(registry.versionResolutionBatches) != 1 {
		t.Fatalf("expected one batched version resolution request, got %#v", registry.versionResolutionBatches)
	}
	if len(registry.versionResolutionBatches[0]) != 2 {
		t.Fatalf("expected both services in the single version resolution batch, got %#v", registry.versionResolutionBatches[0])
	}
	var resolved GenerateSDKRequest
	if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil {
		t.Fatal(err)
	}
	gotVersionIDs := map[uuid.UUID]uuid.UUID{}
	for _, sel := range resolved.Selections {
		if sel.ServiceVersionID == uuid.Nil {
			t.Fatalf("selection missing service_version_id: %#v", sel)
		}
		gotVersionIDs[sel.ServiceID] = sel.ServiceVersionID
	}
	if gotVersionIDs[oktaID] != oktaVersionID || gotVersionIDs[githubID] != githubVersionID {
		t.Fatalf("unexpected resolved service version IDs: %#v", gotVersionIDs)
	}
}

func TestEnsureSDKContractBindingsCurrentRejectsStaleRevision(t *testing.T) {
	serviceID := uuid.New()
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", Revision: 4, SourceHash: "new-hash"},
	}}
	err := ensureSDKContractBindingsCurrent(context.Background(), registry, "fsk_test", []sdkContractBinding{{
		ServiceID: serviceID, Version: "1.0", Revision: 3, SourceHash: "old-hash",
	}})
	if err == nil || err.Error() != "contract_revision_stale" {
		t.Fatalf("expected contract_revision_stale, got %v", err)
	}
}

func TestExecuteSDKConfigApplyRejectsStaleContractBeforeGeneration(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "old-hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID: workspaceID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 4, SourceHash: "new-hash"},
	}}
	proxy := &recordingForwarder{}
	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil || err.Error() != "contract_revision_stale" {
		t.Fatalf("expected stale contract conflict, got %v", err)
	}
	if proxy.forwardCalled || proxy.forwardAndInspectCalled {
		t.Fatal("stale SDK plan must be rejected before generation")
	}
}

func TestExecuteSDKConfigApplyRejectsPinnedVersionRemovedBeforeGeneration(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:       workspaceID,
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "okta", Version: "2.0"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2.0"}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{}
	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil || !strings.Contains(err.Error(), "version 1.0") {
		t.Fatalf("expected removed version conflict, got %v", err)
	}
	if proxy.forwardCalled || proxy.forwardAndInspectCalled {
		t.Fatal("removed SDK version must be rejected before generation")
	}
	if s.listWorkspaceServicesCalls != 0 {
		t.Fatalf("pinned apply should not query service-only workspaceServices, got %d calls", s.listWorkspaceServicesCalls)
	}
}

func TestExecuteSDKConfigApplyRejectsUnpinnedSelection(t *testing.T) {
	serviceID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{workspaceID: workspaceID}
	proxy := &recordingForwarder{}
	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, &mockRegistryClient{}, sdkApplyCall{
		apiKey: "fsk_test", planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil || !strings.Contains(err.Error(), "missing service_version_id") {
		t.Fatalf("expected unpinned selection conflict, got %v", err)
	}
	if s.listWorkspaceServicesCalls != 0 {
		t.Fatalf("unpinned payload should fail before service-only activation lookup, got %d calls", s.listWorkspaceServicesCalls)
	}
	if proxy.forwardCalled || proxy.forwardAndInspectCalled {
		t.Fatal("unpinned SDK payload must be rejected before generation")
	}
}

func TestExecuteSDKConfigApplyRejectsSameNameBucketReplacementBeforeGeneration(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	planID, authorizedBucketID := uuid.New(), uuid.New()
	payload, err := json.Marshal(resolvedSDKPayload(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	}, authorizedBucketID, uuid.Nil, false))
	if err != nil {
		t.Fatalf("marshal resolved payload: %v", err)
	}
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		bucketsByName: map[string]*store.Bucket{"default": {ID: uuid.New(), Name: "default"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{}

	_, err = executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil || !strings.Contains(err.Error(), "bucket identity changed") {
		t.Fatalf("same-name replacement error = %v", err)
	}
	if proxy.forwardCalled || proxy.forwardAndInspectCalled || configStore.artifactApply != nil {
		t.Fatal("stale bucket identity must stop before Registry or local persistence mutation")
	}
}

func TestExecuteSDKConfigApplyPersistsEngineScopeBeforeMarkingApplied(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	workspaceID := uuid.New()
	accountID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, OperationNames: []string{"listLogEvents"}}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID: workspaceID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + endpointID.String() + `"],"operation_names":["listLogEvents"]}]}`}

	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err != nil {
		t.Fatalf("expected apply success, got %v", err)
	}
	if !configStore.markApplied {
		t.Fatal("expected plan marked applied after Engine scope persistence")
	}
	if configStore.artifactApply == nil {
		t.Fatal("expected one atomic artifact config apply")
	}
	saved := configStore.artifactApply.Scope
	if saved.AccountID != accountID || saved.AppID != appID {
		t.Fatalf("unexpected saved scope identity: %#v", saved)
	}
	if result.ExecutionToken == "" {
		t.Fatal("expected Engine to return the one-time execution token")
	}
	assertSavedScopeEndpointSelection(t, saved.Selections, endpointID)
}

func assertSavedScopeEndpointSelection(t *testing.T, payload []byte, endpointID uuid.UUID) {
	t.Helper()
	var savedSelections []models.SDKSelection
	if err := json.Unmarshal(payload, &savedSelections); err != nil {
		t.Fatalf("decode saved selections: %v", err)
	}
	if len(savedSelections) != 1 || len(savedSelections[0].EndpointIDs) != 1 || savedSelections[0].EndpointIDs[0] != endpointID {
		t.Fatalf("expected concrete endpoint selection from Registry result, got %#v", savedSelections)
	}
}

func TestExecuteSDKConfigApplyDoesNotMarkAppliedWhenScopePersistenceFails(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{artifactApplyErr: errors.New("scope db down"), plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:  workspaceID,
		saveScopeErr: errors.New("scope db down"),
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected apply to fail when Engine scope persistence fails")
	}
	if configStore.markApplied {
		t.Fatal("plan must not be marked applied when Engine scope persistence fails")
	}
	if len(proxy.forwardMethods) != 2 || proxy.forwardMethods[1] != http.MethodDelete || proxy.forwardPaths[1] != "/sdk-packages/"+appID.String() {
		t.Fatalf("new Registry artifact was not compensated after persistence failure: methods=%v paths=%v", proxy.forwardMethods, proxy.forwardPaths)
	}
}

func TestExecuteSDKConfigApplyDoesNotDeleteExistingArtifactWhenPersistenceFails(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	planID, accountID, appID := uuid.New(), uuid.New(), uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{artifactApplyErr: errors.New("scope db down"), state: &store.ConfigState{LatestResourceID: &appID}, plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
		serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
	}}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(proxy.forwardMethods) != 1 || proxy.forwardMethods[0] != http.MethodPost {
		t.Fatalf("existing Registry artifact must not be deleted: methods=%v paths=%v", proxy.forwardMethods, proxy.forwardPaths)
	}
}

func TestExecuteSDKConfigApplyRetainsLeaseWhenCompensationIsUnconfirmed(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	planID, accountID := uuid.New(), uuid.New()
	appID := stableAppIDForPlan(planID)
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{artifactApplyErr: errors.New("scope db down"), plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
		serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
	}}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{
		statuses: []int{http.StatusOK, http.StatusGatewayTimeout},
		body:     `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`,
	}
	call := sdkApplyCall{apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash"}

	if _, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, call); err == nil {
		t.Fatal("expected persistence failure")
	}
	if configStore.applyLeaseID == uuid.Nil {
		t.Fatal("unconfirmed Registry delete released the recovery lease")
	}
	methodsAfterFailure := len(proxy.forwardMethods)
	if _, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, call); err == nil || !strings.Contains(err.Error(), "plan_apply_in_progress") {
		t.Fatalf("retry error = %v, want active plan lease", err)
	}
	if len(proxy.forwardMethods) != methodsAfterFailure {
		t.Fatalf("fenced retry reached Registry: methods=%v", proxy.forwardMethods)
	}
}

type sdkGenerationAmbiguityFixture struct {
	configStore *mockConfigStore
	engineStore *workspaceTestStore
	registry    *mockRegistryClient
	call        sdkApplyCall
}

func newSDKGenerationAmbiguityFixture(t *testing.T) sdkGenerationAmbiguityFixture {
	t.Helper()
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	planID, accountID := uuid.New(), uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	return sdkGenerationAmbiguityFixture{
		configStore: &mockConfigStore{plan: &store.ConfigPlan{
			ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
			Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
		}},
		engineStore: &workspaceTestStore{workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		}},
		registry: &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
			serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
		}},
		call: sdkApplyCall{apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash"},
	}
}

func TestExecuteSDKConfigApplyRetainsLeaseForAmbiguousGenerationResponse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "gateway timeout", status: http.StatusGatewayTimeout, body: `{"error":"upstream timeout"}`},
		{name: "malformed success", status: http.StatusOK, body: `not-json`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSDKGenerationAmbiguityFixture(t)
			proxy := &recordingForwarder{status: test.status, body: test.body}

			if _, err := executeSDKConfigApply(context.Background(), fixture.configStore, fixture.engineStore, proxy, fixture.registry, fixture.call); err == nil {
				t.Fatal("expected ambiguous Registry generation failure")
			}
			if fixture.configStore.applyLeaseID == uuid.Nil {
				t.Fatal("ambiguous generation response released the recovery lease")
			}
			if _, err := executeSDKConfigApply(context.Background(), fixture.configStore, fixture.engineStore, proxy, fixture.registry, fixture.call); err == nil || !strings.Contains(err.Error(), "plan_apply_in_progress") {
				t.Fatalf("retry error = %v, want active plan lease", err)
			}
			if len(proxy.forwardMethods) != 1 {
				t.Fatalf("fenced retry reached Registry: methods=%v", proxy.forwardMethods)
			}
		})
	}
}

type serializedSDKApplyStore struct {
	*mockConfigStore
	mutex      sync.Mutex
	applyCalls int
}

func (s *serializedSDKApplyStore) GetConfigState(ctx context.Context, configKey string) (*store.ConfigState, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.mockConfigStore.GetConfigState(ctx, configKey)
}

func (s *serializedSDKApplyStore) ApplyAppConfigPlan(_ context.Context, params store.ApplyAppConfigPlanParams) (*store.ApplyAppConfigPlanResult, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.applyCalls++
	if s.applyCalls > 1 {
		return nil, errors.New("simulated losing persistence race")
	}
	resourceID := *params.Plan.State.LatestResourceID
	s.state = &store.ConfigState{ConfigKey: params.Plan.State.ConfigKey, LatestResourceID: &resourceID}
	return &store.ApplyAppConfigPlanResult{
		State: s.state, AppFamilyID: uuid.New(), AppID: resourceID,
		VersionCreated: true, TokenCreated: true,
	}, nil
}

func TestExecuteSDKConfigApplyConcurrentFirstApplyPreservesWinnerArtifact(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	planID, accountID := uuid.New(), uuid.New()
	appID := stableAppIDForPlan(planID)
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &serializedSDKApplyStore{mockConfigStore: &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}}
	s := &workspaceTestStore{workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
		serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
	}}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}
	call := sdkApplyCall{apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash"}

	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, call)
			errorsSeen <- err
		}()
	}
	close(start)
	var successes, failures int
	for range 2 {
		if <-errorsSeen == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("successes=%d failures=%d", successes, failures)
	}
	for _, method := range proxy.forwardMethods {
		if method == http.MethodDelete {
			t.Fatalf("losing concurrent apply deleted the winner artifact: methods=%v paths=%v", proxy.forwardMethods, proxy.forwardPaths)
		}
	}
}

func TestExecuteSDKConfigApplyCompensatesPendingGenerationFailure(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	planID, accountID := uuid.New(), uuid.New()
	appID := stableAppIDForPlan(planID)
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
		serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
	}}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	pending := `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"pending","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`
	proxy := &recordingForwarder{bodies: []string{pending, "data: {\"type\":\"error\",\"message\":\"generation failed\"}\n\n", ""}}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected pending generation failure")
	}
	wantMethods := []string{http.MethodPost, http.MethodGet, http.MethodDelete}
	if strings.Join(proxy.forwardMethods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("stream failure compensation methods=%v paths=%v", proxy.forwardMethods, proxy.forwardPaths)
	}
}

func TestCompensateNewRegistryArtifactOutlivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	appID := uuid.New()
	proxy := &recordingForwarder{}

	compensateNewRegistryPackage(ctx, proxy, sdkGenerationResult{
		SDKGenerationResult: models.SDKGenerationResult{AppID: appID},
		createdForPlan:      true,
	})

	if len(proxy.forwardMethods) != 1 || proxy.forwardMethods[0] != http.MethodDelete {
		t.Fatalf("canceled caller did not trigger cleanup: methods=%v", proxy.forwardMethods)
	}
	if proxy.forwardContextErrors[0] != nil {
		t.Fatalf("cleanup inherited caller cancellation: %v", proxy.forwardContextErrors[0])
	}
}

func TestRunSDKGenerationDoesNotLogRegistryResponseBody(t *testing.T) {
	const sensitiveMarker = "credential-should-not-enter-logs"
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	_, err := runSDKGeneration(context.Background(), &recordingForwarder{
		status: http.StatusBadRequest,
		body:   `{"error":"` + sensitiveMarker + `"}`,
	}, "fsk_test", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected Registry generation error")
	}
	if strings.Contains(logs.String(), sensitiveMarker) {
		t.Fatalf("Registry response body leaked into logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "status=400") || !strings.Contains(logs.String(), "response_bytes=") {
		t.Fatalf("expected fixed failure metadata, got %q", logs.String())
	}
}

func TestAppApplyPersistenceErrorMapsImmutableBucketConflict(t *testing.T) {
	err := appApplyPersistenceError(context.Background(), store.ErrSDKBucketImmutable, uuid.New())
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusConflict || httpErr.message != "app bucket assignment is immutable" {
		t.Fatalf("immutable bucket error = %#v", err)
	}
}

func TestExecuteSDKConfigApplyReusesExistingScopeCredentialOnRetry(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{state: &store.ConfigState{LatestResourceID: &appID}, plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		existingScopeHash:        "existing-token-hash",
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err != nil {
		t.Fatalf("expected retry apply success, got %v", err)
	}
	if result.ExecutionToken != "" {
		t.Fatalf("retry must not return a rotated one-time token, got %q", result.ExecutionToken)
	}
	if result.ExecutionToken != "" {
		t.Fatalf("retry must not return a rotated one-time token, got %q", result.ExecutionToken)
	}
}

func TestExecuteSDKConfigApplyRejectsExistingScopeOwnedByAnotherAccount(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{artifactApplyErr: store.ErrAppOwnerMismatch, plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		existingScopeHash:        "existing-token-hash",
		existingScopeAccount:     uuid.New(),
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected cross-account existing scope reuse to fail")
	}
	if len(s.savedScopes) != 0 || configStore.markApplied {
		t.Fatalf("cross-account scope must not be saved/applied, saved=%#v applied=%v", s.savedScopes, configStore.markApplied)
	}
}

func TestExecuteSDKConfigApplyAtomicPersistenceFailureDoesNotFinalize(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{artifactApplyErr: errors.New("db temporarily unavailable"), plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected transient scope read error to fail closed")
	}
	if configStore.markApplied || len(s.savedScopes) != 0 {
		t.Fatalf("failed atomic persistence must not finalize or use legacy scope writes: applied=%v saved=%#v", configStore.markApplied, s.savedScopes)
	}
}

func TestValidateRegistryArtifactIdentityRejectsReplacement(t *testing.T) {
	expected := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{AppID: expected})
	if err := validateRegistryAppIdentity(payload, expected); err != nil {
		t.Fatalf("matching artifact identity rejected: %v", err)
	}
	if err := validateRegistryAppIdentity(payload, uuid.New()); err == nil || !strings.Contains(err.Error(), "app_id_mismatch") {
		t.Fatalf("replacement artifact identity was not rejected: %v", err)
	}
}

func TestSDKGenerationPayloadForPlanPreservesCanonicalSourceHash(t *testing.T) {
	planID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	sourceHash := "sha256:" + strings.Repeat("b", 64)
	base, err := json.Marshal(GenerateSDKRequest{Name: "jira-sdk", Version: "2.0.0"})
	if err != nil {
		t.Fatalf("marshal base request: %v", err)
	}
	payload, err := sdkGenerationPayloadForPlan(base, sdkApplyCall{planID: planID}, familyID, appID, sourceHash)
	if err != nil {
		t.Fatalf("sdkGenerationPayloadForPlan() error = %v", err)
	}
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("decode generation payload: %v", err)
	}
	if request.SourceHash != sourceHash || request.AppFamilyID != familyID || request.AppID != appID {
		t.Fatalf("generation identity = %#v, want canonical hash and reserved app identity", request)
	}
	if request.IdempotencyKey != planID.String() || request.GeneratorVersion != models.SDKGeneratorVersion {
		t.Fatalf("generation contract = %#v, want plan idempotency and pinned generator", request)
	}
}

func TestExecuteSDKConfigApplyPendingGenerationFinalizesScopeAfterCompletion(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	endpointID := uuid.New()
	pending := `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"pending","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + endpointID.String() + `"]}]}`
	complete := `data: {"type":"complete","integration_id":"` + appID.String() + `","message":"SDK Generation Complete!"}` + "\n\n"
	proxy := &recordingForwarder{bodies: []string{pending, complete}}

	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err != nil {
		t.Fatalf("pending generation should finalize after completion, got %v", err)
	}
	if result.Status != models.SDKGenerationStatusComplete {
		t.Fatalf("expected complete result, got %#v", result)
	}
	if configStore.artifactApply == nil || !configStore.markApplied {
		t.Fatalf("completed generation must atomically save scope and mark applied, apply=%#v applied=%v", configStore.artifactApply, configStore.markApplied)
	}
	if configStore.upserted == nil || configStore.upserted.LatestResourceID == nil || *configStore.upserted.LatestResourceID != appID {
		t.Fatalf("expected latest resource id %s, got %#v", appID, configStore.upserted)
	}
}

func TestDecodeSDKConfigPlanRejectsRemovedMCPTarget(t *testing.T) {
	body := []byte(`{"source_hash":"config-hash",
		"owner_team":"platform","config_key":"sdk:stripe:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"stripe","version":"1.0.0","language":"typescript","target":"mcp","services":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))

	_, _, err := decodeSDKConfigPlanRequest(req)
	if err == nil || !strings.Contains(err.Error(), "unsupported target") {
		t.Fatalf("expected removed target field to be rejected, got %v", err)
	}
}

func TestDecodeSDKConfigPlanRejectsConfigKeyMismatch(t *testing.T) {
	body := []byte(`{"source_hash":"config-hash",
		"owner_team":"platform","config_key":"sdk:stripe","config":{"apiVersion":"fused/v1","kind":"sdk","name":"stripe","version":"1.0.0","language":"typescript","bucket":"default","services":{"stripe":{"version":"1.0.0","select_all":true}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))

	_, _, err := decodeSDKConfigPlanRequest(req)
	if err == nil || !strings.Contains(err.Error(), `must match "sdk:stripe:1.0.0"`) {
		t.Fatalf("expected config_key mismatch to be rejected, got %v", err)
	}
}

func TestTerminalSDKGenerationEventAcceptsComplete(t *testing.T) {
	body := []byte("event: message\n" + `data: {"type":"complete","message":"done"}` + "\n\n")
	if err := terminalSDKGenerationEvent(body); err != nil {
		t.Fatalf("expected complete stream to succeed, got %v", err)
	}
}

func TestTerminalSDKGenerationEventRejectsError(t *testing.T) {
	body := []byte(`data: {"type":"error","message":"sensitive upstream detail"}` + "\n\n")
	if err := terminalSDKGenerationEvent(body); err == nil || err.Error() != "sdk_generation_failed" {
		t.Fatalf("expected fixed stream error, got %v", err)
	}
}

func TestValidateSDKGenerationResultRequiresAccountIDAndJobStatus(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{Selections: []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}}})
	call := sdkApplyCall{accountID: uuid.New()}
	base := models.SDKGenerationResult{
		AppID:              uuid.New(),
		AccountID:          call.accountID,
		JobID:              "job-1",
		Status:             models.SDKGenerationStatusComplete,
		ScopeSchemaVersion: models.AppScopeSchemaVersion,
		Selections:         models.SDKSelections{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{uuid.New()}}},
	}
	for name, mutate := range map[string]func(*models.SDKGenerationResult){
		"missing account": func(r *models.SDKGenerationResult) { r.AccountID = uuid.Nil },
		"missing job":     func(r *models.SDKGenerationResult) { r.JobID = "" },
		"missing status":  func(r *models.SDKGenerationResult) { r.Status = "" },
	} {
		t.Run(name, func(t *testing.T) {
			result := base
			mutate(&result)
			if err := validateSDKGenerationResult(payload, call, result); err == nil {
				t.Fatal("expected validation to fail closed")
			}
		})
	}
}

func TestValidateGeneratedScopeSelectionsRejectsDuplicateAndExplicitIDDrift(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	planned := []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{endpointID}}}
	tests := map[string][]models.SDKSelection{
		"duplicate service": {
			{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{endpointID}},
			{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{endpointID}},
		},
		"duplicate endpoint": {
			{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{endpointID, endpointID}},
		},
		"explicit endpoint drift": {
			{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{uuid.New()}},
		},
	}
	for name, returned := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateGeneratedScopeSelections(planned, returned); err == nil {
				t.Fatal("expected returned scope to be rejected")
			}
		})
	}
}

func TestValidateGeneratedScopeSelectionsPreservesPortablePolicy(t *testing.T) {
	serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	planned := []models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: []string{"getIssue"},
		AuthType: "oauth2", AuthName: "jiraOAuth", ConnectScopes: []string{"read:jira-work"},
		Injections: []models.SDKInjectionConfig{{Location: "header", Name: "X-Tenant", Value: "$connection.tenant"}},
	}}
	returned := append([]models.SDKSelection(nil), planned...)
	returned[0].EndpointIDs = []uuid.UUID{endpointID}
	if err := validateGeneratedScopeSelections(planned, returned); err != nil {
		t.Fatalf("matching portable policy was rejected: %v", err)
	}
	returned[0].AuthName = "different"
	if err := validateGeneratedScopeSelections(planned, returned); err == nil {
		t.Fatal("Registry auth-policy drift was accepted")
	}
}

// TestValidateGeneratedScopeSelectionsAttachesStructuredMismatchDetail guards
// the observability fix that motivated sdkScopeSelectionMismatchError: a
// production incident showed the engine.sdk_scope.persist trace span logging
// only outcome:"validation_failed" with no missing_scopes/required_scopes
// payload, making the failure undiagnosable from Jaeger alone. This proves
// two things stay true together: (1) errors.As(err, &workspaceConfigHTTPError{})
// still finds the exact same 409/"sdk_scope_selection_mismatch" the HTTP
// layer has always returned (writeSDKConfigError/writeWorkspaceConfigError
// depend on this), and (2) errors.As(err, &sdkScopeSelectionMismatchError{})
// finds the structured Detail a trace span needs -- specifically that
// MissingScopes names the exact endpoint ID the plan required but generation
// didn't return, not just "something didn't match."
func TestValidateGeneratedScopeSelectionsAttachesStructuredMismatchDetail(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	requiredEndpoint := uuid.New()
	returnedEndpoint := uuid.New()
	planned := []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{requiredEndpoint}}}
	returned := []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, EndpointIDs: []uuid.UUID{returnedEndpoint}}}

	err := validateGeneratedScopeSelections(planned, returned)
	if err == nil {
		t.Fatal("expected endpoint drift to be rejected")
	}

	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected errors.As to still find workspaceConfigHTTPError, got %v", err)
	}
	if httpErr.status != http.StatusConflict || httpErr.message != "sdk_scope_selection_mismatch" {
		t.Fatalf("expected unchanged 409/sdk_scope_selection_mismatch, got status=%d message=%q", httpErr.status, httpErr.message)
	}

	var mismatch sdkScopeSelectionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected errors.As to find sdkScopeSelectionMismatchError, got %v", err)
	}
	if mismatch.Detail.Reason != "endpoint_id_drift" {
		t.Fatalf("expected reason endpoint_id_drift, got %q", mismatch.Detail.Reason)
	}
	if mismatch.Detail.ServiceID != serviceID {
		t.Fatalf("expected detail scoped to service %s, got %s", serviceID, mismatch.Detail.ServiceID)
	}
	if len(mismatch.Detail.MissingScopes) != 1 || mismatch.Detail.MissingScopes[0] != requiredEndpoint.String() {
		t.Fatalf("expected missing_scopes to name the exact required-but-absent endpoint %s, got %#v", requiredEndpoint, mismatch.Detail.MissingScopes)
	}
	if len(mismatch.Detail.RequiredScopes) != 1 || mismatch.Detail.RequiredScopes[0] != requiredEndpoint.String() {
		t.Fatalf("expected required_scopes to list what the plan pinned, got %#v", mismatch.Detail.RequiredScopes)
	}
}

func TestExecuteSDKConfigApplyHasNoCompensatingDeleteWhenAtomicFinalizationFails(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{upsertErr: errors.New("state write failed"), plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected finalization failure")
	}
	if len(s.deletedScopes) != 0 {
		t.Fatalf("atomic apply must roll back without a compensating delete, got %#v", s.deletedScopes)
	}
}

func TestExecuteSDKConfigApplyRejectsRegistryScopeMismatch(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	appID := stableAppIDForPlan(planID)
	accountID := uuid.New()
	payload := resolvedDefaultBucketPayload(t, GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"name":"security","version":"1.0.0","language":"typescript","bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID: workspaceID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}},
		},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	otherVersionID := uuid.New()
	proxy := &recordingForwarder{body: `{"app_id":"` + appID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + otherVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, planRevision: 1, sourceHash: "config-hash",
	})
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("expected Registry scope mismatch to fail, got %v", err)
	}
	if len(s.savedScopes) != 0 || configStore.markApplied {
		t.Fatalf("mismatched scope must not be persisted or applied, saved=%#v applied=%v", s.savedScopes, configStore.markApplied)
	}
}

func TestSDKConfigPlanHandler_ResolvesServiceSlugAgainstWorkspaceActivation(t *testing.T) {
	serviceID := uuid.New()
	duplicateNameServiceID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{
				ServiceID:   serviceID,
				ServiceName: "E2E SDK Team Service",
				Version:     "2026-07-01",
			},
			{
				ServiceID:   duplicateNameServiceID,
				ServiceName: "E2E SDK Team Service",
				Version:     "2026-06-01",
			},
		},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID:              {{ServiceID: serviceID, Version: "2026-07-01"}},
			duplicateNameServiceID: {{ServiceID: duplicateNameServiceID, Version: "2026-06-01"}},
		},
	}
	configStore := &mockConfigStore{}
	registryClient := &mockRegistryClient{slugIDs: map[string]uuid.UUID{"e2e_sdk_team_service": serviceID}}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
            "bucket": "default",
            "services": {
				"e2e_sdk_team_service": {
					"version": "2026-07-01",
					"operations": ["listUsers"]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(registryClient.slugBatches) != 1 || len(registryClient.slugBatches[0]) != 1 {
		t.Fatalf("expected one slug batch, got %#v", registryClient.slugBatches)
	}
	var resolved GenerateSDKRequest
	if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil {
		t.Fatalf("decode resolved payload: %v", err)
	}
	if resolved.Selections[0].ServiceID != serviceID {
		t.Fatalf("expected service %s, got %s", serviceID, resolved.Selections[0].ServiceID)
	}
}

// TestSDKConfigPlanHandler_BatchesVersionLookups is a regression guard for
// the N+1 fix: an SDK config referencing multiple services must fetch every
// referenced service's allowed versions in one batched call, not one
// ListWorkspaceServiceVersions call per service.
func TestSDKConfigPlanHandler_BatchesVersionLookups(t *testing.T) {
	oktaID := uuid.New()
	githubID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: oktaID, ServiceName: "okta", Version: "2026-07-01"},
			{ServiceID: githubID, ServiceName: "github", Version: "2026-06-15"},
		},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			oktaID:   {{ServiceID: oktaID, Version: "2026-07-01"}},
			githubID: {{ServiceID: githubID, Version: "2026-06-15"}},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, &mockRegistryClient{}))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
            "bucket": "default",
            "services": {
				"okta": {"version": "2026-07-01", "operations": ["listLogEvents"]},
				"github": {"version": "2026-06-15", "operations": ["listRepositories"]}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.batchedVersionLookups) != 1 {
		t.Fatalf("expected exactly one batched version lookup, got %d: %#v", len(s.batchedVersionLookups), s.batchedVersionLookups)
	}
	if got, want := len(s.batchedVersionLookups[0]), 2; got != want {
		t.Fatalf("expected the batched lookup to cover both referenced services, got %d ids: %#v", got, s.batchedVersionLookups[0])
	}
}

// TestSDKConfigPlanHandler_BatchesOperationValidation is a regression guard
// for the Registry N+1 fix: validating several selected operationIds for one
// service must call endpointsByNames once, not endpointByName once per
// operation.
func TestSDKConfigPlanHandler_BatchesOperationValidation(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   serviceID,
			ServiceName: "okta",
			Version:     "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-07-01"}},
		},
	}
	registryClient := &mockRegistryClient{}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
            "bucket": "default",
            "services": {
				"okta": {
					"version": "2026-07-01",
					"operations": ["listLogEvents", "getUser", "deleteUser"]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(registryClient.validatedSelections) != 1 {
		t.Fatalf("expected one batched validation, got %d", len(registryClient.validatedSelections))
	}
	if got, want := len(registryClient.validatedSelections[0][0].OperationNames), 3; got != want {
		t.Fatalf("expected the batch to cover all 3 operations, got %d names: %#v", got, registryClient.validatedSelections[0][0].OperationNames)
	}
}

func TestSDKConfigPlanHandler_ReturnsRelevantNotifications(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	serviceID := uuid.New()
	otherServiceID := uuid.New()
	workspaceID := uuid.New()
	noteID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   serviceID,
			ServiceName: "okta",
			Version:     "2026-07-01",
		}, {
			ServiceID:   otherServiceID,
			ServiceName: "github",
			Version:     "2026-06-15",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-07-01"}},
		},
	}
	configStore := &mockConfigStore{
		notifications: []store.WorkspaceNotification{{
			ID:        noteID,
			Type:      store.WorkspaceNotificationTypeVersionRemoved,
			Severity:  store.WorkspaceNotificationSeverityBreaking,
			Status:    store.WorkspaceNotificationStatusPending,
			ServiceID: &serviceID,
			Version:   "2026-07-01",
			ConfigKey: "sdk:security:1.0.0",
			Message:   "okta version removed",
		}, {
			ID:        uuid.New(),
			Type:      store.WorkspaceNotificationTypeServiceRemoved,
			Severity:  store.WorkspaceNotificationSeverityBreaking,
			Status:    store.WorkspaceNotificationStatusPending,
			ServiceID: &otherServiceID,
			ConfigKey: "sdk:other",
			Message:   "other service removed",
		}},
	}
	registryClient := &mockRegistryClient{
		driftSnapshots: []models.DriftSnapshot{{
			ID:                  uuid.New(),
			IntegrationObjectID: uuid.New(),
			Status:              "pending",
			Diff: models.DriftChanges{{
				Field:       "path",
				Severity:    "breaking",
				Description: "path changed",
			}},
		}},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
            "bucket": "default",
            "services": {
				"okta": {
					"version": "2026-07-01",
					"operations": ["listLogEvents"]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Notifications struct {
			Items []workspaceNotificationInboxItem `json:"items"`
		} `json:"notifications"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(registryClient.driftServiceIDs) != 1 || registryClient.driftServiceIDs[0] != serviceID {
		t.Fatalf("expected one SDK-relevant Registry drift query, got %#v", registryClient.driftServiceIDs)
	}
	if len(resp.Notifications.Items) != 2 {
		t.Fatalf("expected engine + registry notification, got %#v", resp.Notifications.Items)
	}
	if resp.Notifications.Items[0].ID != "engine:"+noteID.String() {
		t.Fatalf("expected relevant engine notification first, got %#v", resp.Notifications.Items[0])
	}
	if resp.Notifications.Items[1].Source != "registry" || resp.Notifications.Items[1].IntegrationObjectID == "" {
		t.Fatalf("expected enriched registry drift notification, got %#v", resp.Notifications.Items[1])
	}
}

func TestFilterSDKEngineNotificationsTargetsExactConfig(t *testing.T) {
	current := "sdk:security:2.0.0"
	notifications := []store.WorkspaceNotification{
		{ID: uuid.New(), ConfigKey: current, Message: "exact"},
		{ID: uuid.New(), ConfigKey: "sdk:security:1.0.0", Message: "other version"},
		{ID: uuid.New(), ConfigKey: "", Message: "workspace-wide"},
		{ID: uuid.New(), ConfigKey: "sdk:other:1.0.0, " + current, Message: "multiple configs"},
	}

	items := filterSDKEngineNotifications(notifications, current)
	if len(items) != 2 {
		t.Fatalf("notifications = %#v, want exact and multi-targeted only", items)
	}
	if items[0].Message != "exact" || items[1].Message != "multiple configs" {
		t.Fatalf("unexpected targeted notifications: %#v", items)
	}
}

func TestSDKConfigPlanHandler_RejectsLegacyEndpoints(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   serviceID,
			ServiceName: "okta",
			Version:     "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-07-01"}},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, nil))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team":"platform",
		"config_key": "sdk:security:1.0.0",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "sdk",
			"name": "security",
			"version": "1.0.0",
			"language": "typescript",
            "bucket": "default",
            "services": {
				"okta": {
					"version": "2026-07-01",
					"endpoints": ["listLogEvents"]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestArtifactApplyRejectsPlanRevisionChangedAfterAuthorization(t *testing.T) {
	for _, configType := range []store.ConfigType{store.ConfigTypeSDK, store.ConfigTypeMCP, store.ConfigTypeWebhook} {
		t.Run(string(configType), func(t *testing.T) {
			plan := &store.ConfigPlan{
				ID: uuid.New(), ConfigKey: string(configType) + ":revision-drift", ConfigType: configType,
				Status: store.ConfigPlanStatusPending, SourceHash: "source", Revision: 2,
			}
			configStore := &mockConfigStore{plan: plan}
			call := sdkApplyCall{planID: plan.ID, planRevision: 1, sourceHash: plan.SourceHash}
			var err error
			if configType == store.ConfigTypeSDK {
				_, _, err = loadSDKPlanForApply(context.Background(), configStore, call)
			} else {
				_, _, err = loadConfigPlanForApply(context.Background(), configStore, call, configType)
			}
			var httpErr workspaceConfigHTTPError
			if !errors.As(err, &httpErr) || httpErr.status != http.StatusConflict || httpErr.message != "plan_revision_changed" {
				t.Fatalf("revision drift error = %#v, want 409 plan_revision_changed", err)
			}
		})
	}
}

func TestArtifactApplyHandlersRejectMissingAuthorizedPlanRevision(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{name: "sdk", handler: SDKConfigApplyHandler(nil, nil, nil)},
		{name: "mcp", handler: MCPConfigApplyHandler(nil, nil, nil)},
		{name: "webhook", handler: WebhookConfigApplyHandler(nil, nil, nil, nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "webhook" {
				entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{WebhookIngestionEnabled: true})
				defer entitlement.LiveEntitlement.Reset()
			}
			request := httptest.NewRequest(http.MethodPost, "/"+test.name+"-config/apply", strings.NewReader(`{"plan_id":"`+uuid.NewString()+`"}`))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), accesscontrol.Actor{AccountID: uuid.New()}))
			response := httptest.NewRecorder()

			test.handler.ServeHTTP(response, request)

			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "authorized plan revision unavailable") {
				t.Fatalf("missing revision response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

// A persisted false value from the former Dev-plan gate is normalized to true,
// so upgrading an Engine makes drift available without manual intervention.
func TestCollectSDKPlanNotifications_LegacyDriftFalseStillCallsRegistry(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: false})
	defer entitlement.LiveEntitlement.Reset()

	serviceID := uuid.New()
	configStore := &mockConfigStore{
		notifications: []store.WorkspaceNotification{{
			ID:        uuid.New(),
			Type:      store.WorkspaceNotificationTypeVersionRemoved,
			ConfigKey: "sdk:test:1.0.0",
			Message:   "okta version removed",
		}},
	}
	registryClient := &mockRegistryClient{
		driftSnapshots: []models.DriftSnapshot{{ID: uuid.New()}},
	}

	inbox := collectSDKPlanNotifications(context.Background(), configStore, registryClient, sdkPlanCall{
		apiKey:  "test-key",
		request: SDKConfigPlanRequest{ConfigKey: "sdk:test:1.0.0"},
	}, []sdkResolvedService{{ServiceID: serviceID, Version: "v1"}})

	if len(registryClient.driftServiceIDs) != 1 || registryClient.driftServiceIDs[0] != serviceID {
		t.Fatalf("expected registry drift call after legacy value normalization, got %#v", registryClient.driftServiceIDs)
	}
	if len(inbox.Items) != 2 {
		t.Fatalf("expected engine and registry items, got %#v", inbox.Items)
	}
}

// TestCollectSDKPlanNotifications_DriftMonitoringEnabled_CallsRegistry
// verifies the positive path still reaches the registry when enabled.
func TestCollectSDKPlanNotifications_DriftMonitoringEnabled_CallsRegistry(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	serviceID := uuid.New()
	configStore := &mockConfigStore{
		notifications: []store.WorkspaceNotification{},
	}
	registryClient := &mockRegistryClient{
		driftSnapshots: []models.DriftSnapshot{{ID: uuid.New(), IntegrationObjectID: uuid.New()}},
	}

	inbox := collectSDKPlanNotifications(context.Background(), configStore, registryClient, sdkPlanCall{
		apiKey:  "test-key",
		request: SDKConfigPlanRequest{ConfigKey: "sdk:test:1.0.0"},
	}, []sdkResolvedService{{ServiceID: serviceID, Version: "v1"}})

	if len(registryClient.driftServiceIDs) != 1 || registryClient.driftServiceIDs[0] != serviceID {
		t.Fatalf("expected registry drift call when enabled, got %#v", registryClient.driftServiceIDs)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Source != "registry" {
		t.Fatalf("expected registry drift item, got %#v", inbox.Items)
	}
}

var _ = models.SDKSelection{}
