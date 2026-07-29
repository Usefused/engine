package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

func TestCanonicalArtifactStateIgnoresSetOrdering(t *testing.T) {
	first, err := canonicalArtifactState(sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "mcp", Name: "reader", Version: "1.0.0",
		Services: map[string]sdkConfigServiceDoc{
			"github": {Version: "2026-07-01", Operations: []string{"getUser", "listRepos", "getUser"}, Connect: &sdkArtifactConnectDoc{Scopes: []string{"repo", "read:user"}}},
		},
	})
	if err != nil {
		t.Fatalf("canonicalArtifactState(first): %v", err)
	}
	second, err := canonicalArtifactState(sdkConfigDocument{
		APIVersion: " fused/v1 ", Kind: "mcp", Name: " reader ", Version: "1.0.0",
		Services: map[string]sdkConfigServiceDoc{
			"github": {Version: " 2026-07-01 ", Operations: []string{"listRepos", "getUser"}, Connect: &sdkArtifactConnectDoc{Scopes: []string{"read:user", "repo"}}},
		},
	})
	if err != nil {
		t.Fatalf("canonicalArtifactState(second): %v", err)
	}
	if !sameCanonicalArtifactState(first, second) {
		t.Fatalf("equivalent artifact configs must share canonical state: %s != %s", first, second)
	}
}

func TestArtifactResolvedPayloadHasNoTarget(t *testing.T) {
	payload, err := json.Marshal(artifactResolvedPayload{})
	if err != nil {
		t.Fatalf("marshal artifact payload: %v", err)
	}
	if strings.Contains(string(payload), "target") {
		t.Fatalf("MCP resolved payload must not carry a target: %s", payload)
	}
}

func TestValidateArtifactBucketReadinessReportsEveryMissingOAuthService(t *testing.T) {
	bucketID := uuid.New()
	first, second := uuid.New(), uuid.New()
	s := &workspaceTestStore{
		bucketsByName: map[string]*store.Bucket{
			"production": {ID: bucketID},
		},
	}
	err := validateArtifactBucketReadiness(context.Background(), s, "production", []models.SDKSelection{
		{ServiceID: first, AuthType: "oauth"}, {ServiceID: second, AuthType: "oidc"},
	})
	if err == nil || !strings.Contains(err.Error(), first.String()) || !strings.Contains(err.Error(), second.String()) {
		t.Fatalf("expected one aggregated readiness error containing both services, got %v", err)
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
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
	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, nil))
	body := []byte(`{"source_hash":"config-hash","config_key":"sdk:security:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"security","version":"1.0.0","language":"typescript","bucket":"default","services":{"okta":{"version":"1.0","operations":["listLogEvents"]}}}}`)
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
	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registry))
	body := []byte(`{"source_hash":"config-hash","config_key":"sdk:security:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"security","version":"1.0.0","language":"typescript","bucket":"default","services":{"okta":{"version":"1.0","operations":["listLogEvents"]}}}}`)
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
	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registry))
	body := []byte(`{"source_hash":"config-hash","config_key":"sdk:security:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"security","version":"1.0.0","language":"typescript","bucket":"default","services":{"okta":{"version":"1.0","operations":["listLogEvents"]},"github":{"version":"2.0","operations":["listRepos"]}}}}`)
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
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "old-hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
		apiKey: "fsk_test", planID: planID, sourceHash: "config-hash",
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
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
		apiKey: "fsk_test", planID: planID, sourceHash: "config-hash",
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
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{workspaceID: workspaceID}
	proxy := &recordingForwarder{}
	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, &mockRegistryClient{}, sdkApplyCall{
		apiKey: "fsk_test", planID: planID, sourceHash: "config-hash",
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

func TestExecuteSDKConfigApplyPersistsEngineScopeBeforeMarkingApplied(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	workspaceID := uuid.New()
	accountID := uuid.New()
	planID := uuid.New()
	artifactID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, OperationNames: []string{"listLogEvents"}}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + endpointID.String() + `"]}]}`}

	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
	})
	if err != nil {
		t.Fatalf("expected apply success, got %v", err)
	}
	if !configStore.markApplied {
		t.Fatal("expected plan marked applied after Engine scope persistence")
	}
	if len(s.savedScopes) != 1 {
		t.Fatalf("expected one Engine scope save, got %#v", s.savedScopes)
	}
	saved := s.savedScopes[0]
	if saved.accountID != accountID || saved.artifactID != artifactID {
		t.Fatalf("unexpected saved scope identity: %#v", saved)
	}
	if result.ExecutionToken == "" {
		t.Fatal("expected Engine to return the one-time execution token")
	}
	assertSavedScopeEndpointSelection(t, saved.selections, endpointID)
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
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected apply to fail when Engine scope persistence fails")
	}
	if configStore.markApplied {
		t.Fatal("plan must not be marked applied when Engine scope persistence fails")
	}
}

func TestExecuteSDKConfigApplyReusesExistingScopeCredentialOnRetry(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
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
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected cross-account existing scope reuse to fail")
	}
	if len(s.savedScopes) != 0 || configStore.markApplied {
		t.Fatalf("cross-account scope must not be saved/applied, saved=%#v applied=%v", s.savedScopes, configStore.markApplied)
	}
}

func TestExecuteSDKConfigApplyTransientScopeReadFailureDoesNotIssueCredential(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		getScopeErr:              errors.New("db temporarily unavailable"),
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected transient scope read error to fail closed")
	}
	if len(s.savedScopes) != 0 {
		t.Fatalf("transient read error must not create/overwrite scope, got %#v", s.savedScopes)
	}
}

func TestExecuteSDKConfigApplyPendingGenerationFinalizesScopeAfterCompletion(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
	pending := `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"pending","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + endpointID.String() + `"]}]}`
	complete := `data: {"type":"complete","integration_id":"` + artifactID.String() + `","message":"SDK Generation Complete!"}` + "\n\n"
	proxy := &recordingForwarder{bodies: []string{pending, complete}}

	result, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
	})
	if err != nil {
		t.Fatalf("pending generation should finalize after completion, got %v", err)
	}
	if result.Status != models.SDKGenerationStatusComplete {
		t.Fatalf("expected complete result, got %#v", result)
	}
	if len(s.savedScopes) != 1 || !configStore.markApplied {
		t.Fatalf("completed generation must save scope and mark applied, saved=%#v applied=%v", s.savedScopes, configStore.markApplied)
	}
	if configStore.upserted == nil || configStore.upserted.LatestResourceID == nil || *configStore.upserted.LatestResourceID != artifactID {
		t.Fatalf("expected latest resource id %s, got %#v", artifactID, configStore.upserted)
	}
}

func TestDecodeSDKConfigPlanRejectsRemovedMCPTarget(t *testing.T) {
	body := []byte(`{"source_hash":"config-hash","config_key":"sdk:stripe:1.0.0","config":{"apiVersion":"fused/v1","kind":"sdk","name":"stripe","version":"1.0.0","language":"typescript","target":"mcp","services":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))

	_, _, err := decodeSDKConfigPlanRequest(req)
	if err == nil || !strings.Contains(err.Error(), "unsupported target") {
		t.Fatalf("expected removed target field to be rejected, got %v", err)
	}
}

func TestDecodeSDKConfigPlanRejectsConfigKeyMismatch(t *testing.T) {
	body := []byte(`{"source_hash":"config-hash","config_key":"sdk:stripe","config":{"apiVersion":"fused/v1","kind":"sdk","name":"stripe","version":"1.0.0","language":"typescript","bucket":"default","services":{"stripe":{"version":"1.0.0","select_all":true}}}}`)
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
	body := []byte(`data: {"type":"error","message":"generation failed"}` + "\n\n")
	if err := terminalSDKGenerationEvent(body); err == nil || !strings.Contains(err.Error(), "generation failed") {
		t.Fatalf("expected stream error to propagate, got %v", err)
	}
}

func TestValidateSDKGenerationResultRequiresAccountIDAndJobStatus(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{Selections: []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}}})
	call := sdkApplyCall{accountID: uuid.New()}
	base := models.SDKGenerationResult{
		ArtifactID:         uuid.New(),
		AccountID:          call.accountID,
		JobID:              "job-1",
		Status:             models.SDKGenerationStatusComplete,
		ScopeSchemaVersion: models.ArtifactScopeSchemaVersion,
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

func TestExecuteSDKConfigApplyDeletesNewScopeWhenFinalizationFails(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{upsertErr: errors.New("state write failed"), plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
	}}
	s := &workspaceTestStore{
		workspaceID:              workspaceID,
		accountID:                accountID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"},
	}}
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
	})
	if err == nil {
		t.Fatal("expected finalization failure")
	}
	if len(s.deletedScopes) != 1 || s.deletedScopes[0] != artifactID {
		t.Fatalf("expected newly saved scope to be compensated, got %#v", s.deletedScopes)
	}
}

func TestExecuteSDKConfigApplyRejectsRegistryScopeMismatch(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	workspaceID := uuid.New()
	planID := uuid.New()
	artifactID := uuid.New()
	accountID := uuid.New()
	payload, _ := json.Marshal(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: serviceVersionID}},
		ContractBindings: []sdkContractBinding{{ServiceID: serviceID, Version: "1.0", ServiceVersionID: serviceVersionID, Revision: 3, SourceHash: "hash"}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, SourceHash: "config-hash",
		BaseGeneration: 0, Status: store.ConfigPlanStatusPending, DesiredState: json.RawMessage(`{"bucket":"default"}`), ResolvedPayload: payload,
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
	proxy := &recordingForwarder{body: `{"artifact_id":"` + artifactID.String() + `","account_id":"` + accountID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":2,"selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + otherVersionID.String() + `","endpoint_ids":["` + uuid.NewString() + `"]}]}`}

	_, err := executeSDKConfigApply(context.Background(), configStore, s, proxy, registry, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, planID: planID, sourceHash: "config-hash",
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, &mockRegistryClient{}))

	body := []byte(`{
		"source_hash": "abc",
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
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
			ConfigKey: "sdk:security",
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registryClient))

	body := []byte(`{
		"source_hash": "abc",
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

	r := chi.NewRouter()
	r.Post("/sdk-config/plan", SDKConfigPlanHandler(&mockConfigStore{}, s, nil))

	body := []byte(`{
		"source_hash": "abc",
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

func TestSDKConfigDownloadHandler_BlocksRemovedVersion(t *testing.T) {
	serviceID := uuid.New()
	artifactID := uuid.New()
	workspaceID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   serviceID,
			ServiceName: "okta",
			Version:     "2026-08-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-08-01"}},
		},
	}
	configStore := &mockConfigStore{
		state: &store.ConfigState{
			ConfigKey:        "sdk:security",
			LatestResourceID: &artifactID,
			DesiredState: json.RawMessage(`{
				"kind": "sdk",
				"name": "security",
				"services": {
					"okta": {
						"version": "2026-07-01",
						"operations": ["listLogEvents"]
					}
				}
			}`),
		},
	}
	proxy := &mockForwarder{}

	r := chi.NewRouter()
	r.Get("/sdk-config/{name}/download", SDKConfigDownloadHandler(configStore, s, proxy))

	req := httptest.NewRequest(http.MethodGet, "/sdk-config/security/download", nil)
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if proxy.called {
		t.Fatal("download must not proxy when the SDK config references a removed version")
	}
}

var _ = models.SDKSelection{}
