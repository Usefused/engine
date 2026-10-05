package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// availableAuthFixture supplies two provider-supported alternatives without any credential values.
func availableAuthFixture() (uuid.UUID, sandbox.ServiceVersionExecutionAuthContract) {
	id := uuid.New()
	return id, executionAuthContract(id, fusedobject.AuthConfigs{
		{Name: "basicAuth", Type: "http", Scheme: "basic"},
		{Name: "bearerAuth", Type: "http", Scheme: "bearer"},
	}, securedOperationAlternatives("create", []string{"basicAuth"}, []string{"bearerAuth"}))
}

// TestAvailableAppAuthSelection exercises metadata-only choice, explicit overrides, absence, ambiguity and exact bucket isolation.
func TestAvailableAppAuthSelection(t *testing.T) {
	for _, test := range []struct {
		name         string
		keys         []string
		explicit     bool
		override     bool
		wrongService bool
		failure      bool
		want         string
		wantError    string
	}{
		{name: "bearer only", keys: []string{"bearerAuth"}, want: "bearerAuth"},
		{name: "basic only", keys: []string{"basicAuth_username", "basicAuth_password"}, want: "basicAuth"},
		{name: "none retains warning policy", want: "basicAuth"},
		{name: "partial basic does not compete", keys: []string{"basicAuth_username", "bearerAuth"}, want: "bearerAuth"},
		{name: "multiple complete choices", keys: []string{"basicAuth_username", "basicAuth_password", "bearerAuth"}, wantError: "app_auth_selection_required"},
		{name: "explicit basic stays basic", keys: []string{"bearerAuth"}, explicit: true, want: "basicAuth"},
		{name: "override cannot use default credentials", keys: []string{"bearerAuth"}, override: true, want: "basicAuth"},
		{name: "other service cannot supply credential", keys: []string{"bearerAuth"}, wrongService: true, want: "basicAuth"},
		{name: "storage failure cannot mean absent", failure: true, wantError: "failed to read bucket readiness"},
	} {
		// Each case owns its metadata and exact service/bucket identities.
		t.Run(test.name, func(t *testing.T) {
			id, contract := availableAuthFixture()
			bucket := store.Bucket{ID: uuid.New(), Name: "default"}
			s := &workspaceTestStore{secretMetas: map[uuid.UUID][]store.WorkspaceSecretMeta{}}
			credentialService := id
			// A same-named key in another service is not an auth candidate.
			if test.wrongService {
				credentialService = uuid.New()
			}
			for _, key := range test.keys {
				s.secretMetas[bucket.ID] = append(s.secretMetas[bucket.ID], store.WorkspaceSecretMeta{ServiceID: credentialService, KeyName: key})
			}
			// Store outages must be propagated before a reviewed choice can be made.
			if test.failure {
				s.appBucketReadinessErr = errors.New("offline")
			}
			service := sdkConfigServiceDoc{Version: "v1", Operations: []string{"create"}}
			selection := models.SDKSelection{ServiceID: id, OperationNames: []string{"create"}}
			// An authored selector takes precedence even when its credential is absent.
			if test.explicit {
				service.Auth = &sdkAppAuthDoc{Type: "basic", Name: "basicAuth"}
				selection.AuthType, selection.AuthName = "basic", "basicAuth"
			}
			// Resolve the provider policy first, as the shared production admission path does.
			if err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{}); err != nil {
				t.Fatal(err)
			}
			contract.OperationNames = selection.OperationNames
			contracts := map[string]sandbox.ServiceVersionExecutionAuthContract{executionAuthContractKey(id, "v1", selection.OperationNames, false): contract}
			buckets := appBucketSet{Default: bucket}
			// Service overrides must never search the default bucket as a fallback.
			if test.override {
				buckets.Overrides = map[uuid.UUID]store.Bucket{id: {ID: uuid.New(), Name: "other"}}
			}
			selections := []models.SDKSelection{selection}
			err := resolveAvailableAppAuth(context.Background(), s, sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{"stripe": service}}, sdkConfigDocument{}, []sdkResolvedService{{ServiceID: id, Version: "v1", ServiceName: "Stripe", PublicTarget: "stripe"}}, selections, contracts, buckets)
			assertAvailableAuthResult(t, selections[0], err, test.want, test.wantError)
			// Explicit selections need no availability query; automatic selection batches all alternatives once.
			wantQueries := 1
			if test.explicit {
				wantQueries = 0
			}
			assertAuthAvailabilityQueries(t, s, wantQueries)
		})
	}
}

// assertAuthAvailabilityQueries prevents broad secret reads and per-candidate database queries.
func assertAuthAvailabilityQueries(t *testing.T, s *workspaceTestStore, want int) {
	t.Helper()
	// Metadata is requested once per bucket, never by listing or decrypting all secrets.
	if s.appBucketReadinessCalls != want || s.secretMetaCalls != 0 {
		t.Fatalf("queries exact=%d broad=%d", s.appBucketReadinessCalls, s.secretMetaCalls)
	}
}

// assertAvailableAuthResult checks typed ambiguity separately from storage failures and successful scheme selection.
func assertAvailableAuthResult(t *testing.T, selection models.SDKSelection, err error, want, wantError string) {
	t.Helper()
	// Error cases must never turn into an arbitrary successful selection.
	if wantError != "" {
		var httpErr workspaceConfigHTTPError
		if !errors.As(err, &httpErr) || (httpErr.code != wantError && !strings.Contains(httpErr.message, wantError)) {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	// Successful selection must persist the named policy used by execution.
	if err != nil || selection.AuthName != want {
		t.Fatalf("selection=%s error=%v, want=%s", selection.AuthName, err, want)
	}
}

// TestAvailableAuthPreservesSecurityRequirements rejects incomplete AND branches and leaves anonymous selections credential-free.
func TestAvailableAuthPreservesSecurityRequirements(t *testing.T) {
	id, contract := availableAuthFixture()
	contract.AuthConfigs = append(contract.AuthConfigs, fusedobject.AuthConfig{Name: "secondKey", Type: "apiKey"})
	contract.Operations = []sandbox.OperationSecuritySummary{securedOperationAlternatives("create", []string{"bearerAuth", "secondKey"}, []string{"basicAuth"})}
	candidates, err := availableAppAuthCandidates(models.SDKSelection{ServiceID: id}, contract)
	// Both members of the AND branch remain required, and its two names must not create false ambiguity.
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates=%#v error=%v", candidates, err)
	}
	ready := map[string]bool{id.String() + "\x00bearerAuth": true}
	// The available bearer token alone cannot satisfy its companion key requirement.
	for _, candidate := range candidates {
		if len(missingAppBucketMaterial(candidate, nil, nil, ready)) == 0 {
			t.Fatal("partial credentials satisfied a complete policy")
		}
	}
	contract.Operations = []sandbox.OperationSecuritySummary{anonymousOperation("create")}
	candidates, err = availableAppAuthCandidates(models.SDKSelection{ServiceID: id}, contract)
	// Provider-supported anonymous access must not read or select credentials.
	if err != nil || len(candidates) != 0 {
		t.Fatalf("anonymous candidates=%#v error=%v", candidates, err)
	}
}

// TestCredentialAwareAppPlans pins inferred auth through each public adapter's HTTP planning boundary.
func TestCredentialAwareAppPlans(t *testing.T) {
	for _, kind := range []string{"sdk", "mcp", "unified_app"} {
		// All adapters must return the same credential-backed choice without provider execution.
		t.Run(kind, func(t *testing.T) {
			id, contract := availableAuthFixture()
			versionID := uuid.New()
			s := &workspaceTestStore{accountID: uuid.New(), workspaceServices: []store.WorkspaceService{{ServiceID: id, ServiceName: "Stripe", Version: "v1"}}, workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{id: {{ServiceID: id, ServiceVersionID: versionID, Version: "v1"}}}, secretMetas: map[uuid.UUID][]store.WorkspaceSecretMeta{workspaceTestBucketID("default"): {{ServiceID: id, KeyName: "bearerAuth"}}}}
			registry := &appAuthMismatchRegistry{mockRegistryClient: &mockRegistryClient{slugIDs: map[string]uuid.UUID{"stripe": id}, contractRevisions: map[string]sandbox.ServiceVersionRevision{id.String() + "|v1": {ServiceID: id, ServiceVersionID: versionID, Version: "v1", Revision: 1}}}, contracts: []sandbox.ServiceVersionExecutionAuthContract{contract}}
			configs := &mockConfigStore{}
			router := newControlTestRouter(s.accountID)
			router.Post("/sdk-config/plan", SDKConfigPlanHandler(configs, s, registry))
			router.Post("/mcp-config/plan", MCPConfigPlanHandler(configs, s, registry))
			router.Post("/unified-app-config/plan", UnifiedAppConfigPlanHandler(configs, s, registry))
			config := map[string]any{"apiVersion": "fused/v1", "kind": kind, "name": "auth-choice", "version": "1.0.0", "bucket": "default", "services": map[string]any{"stripe": map[string]any{"version": "v1", "operations": []string{"create"}}}}
			// Each adapter retains its own config vocabulary while sharing auth resolution.
			if kind == "sdk" {
				config["language"] = "typescript"
			} else {
				config["description"] = "Test credentials"
			}
			// A precompiled Unified App tests auth planning independently of worker isolation availability.
			if kind == "unified_app" {
				config["bundle_digest"] = store.UnifiedAppBundleDigest([]byte("auth-choice-fixture"))
			}

			body, _ := json.Marshal(map[string]any{"source_hash": "fixture", "config_key": kind + ":auth-choice:1.0.0", "config": config})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/"+strings.ReplaceAll(kind, "_", "-")+"-config/plan", strings.NewReader(string(body))))
			// HTTP clients must receive a usable reviewed plan, not a missing-Basic warning.
			if response.Code != http.StatusOK || configs.createdPlan == nil {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			assertCredentialAwarePlan(t, configs.createdPlan, response.Body.String())
			assertReviewedAuthSurvivesCredentialChange(t, s, configs.createdPlan)
		})
	}
}

// assertReviewedAuthSurvivesCredentialChange proves apply uses reviewed scope instead of inferring from later credential availability.
func assertReviewedAuthSurvivesCredentialChange(t *testing.T, s *workspaceTestStore, plan *store.CreateConfigPlanParams) {
	t.Helper()
	s.secretMetas = nil
	before := s.appBucketReadinessCalls
	_, decoded, err := decodeAppApplyPlan(context.Background(), &mockConfigStore{}, s, &store.ConfigPlan{DesiredState: plan.DesiredState, ResolvedPayload: plan.ResolvedPayload}, "App")
	// Removing the token after planning must not change auth or trigger a second selection lookup.
	if err != nil || len(decoded.Selections) != 1 || decoded.Selections[0].AuthName != "bearerAuth" || s.appBucketReadinessCalls != before {
		t.Fatalf("apply selection=%#v error=%v queries=%d", decoded.Selections, err, s.appBucketReadinessCalls-before)
	}
}

// assertCredentialAwarePlan verifies the review and persisted execution scope agree on the selected scheme.
func assertCredentialAwarePlan(t *testing.T, plan *store.CreateConfigPlanParams, response string) {
	t.Helper()

	var payload appResolvedPayload
	var desired sdkConfigDocument
	_ = json.Unmarshal(plan.ResolvedPayload, &payload)
	_ = json.Unmarshal(plan.DesiredState, &desired)
	// The execution snapshot and editable config must agree on the exact inferred scheme.
	if len(payload.Selections) != 1 || payload.Selections[0].AuthName != "bearerAuth" || desired.Services["Stripe"].Auth == nil || desired.Services["Stripe"].Auth.Name != "bearerAuth" {
		t.Fatalf("payload=%#v desired=%#v", payload, desired)
	}
	// Existing review surfaces show the resolved choice while no missing-credential warning remains.
	if !strings.Contains(response, `"name":"bearerAuth"`) || strings.Contains(response, "bucket_credentials_missing") {
		t.Fatalf("review=%s", response)
	}
	retained := retainPlannedAppAuth(sdkConfigServiceDoc{}, desired.Services["Stripe"])
	// Replanning the same immutable version must retain its choice after credentials change.
	if retained.Auth == nil || retained.Auth.Name != "bearerAuth" {
		t.Fatalf("retained=%#v", retained)
	}
}

// TestPublishedAuthReviewPreservesDesiredState keeps re-planning from rewriting old immutable configs or type-only selectors.
func TestPublishedAuthReviewPreservesDesiredState(t *testing.T) {
	id := uuid.New()
	services := []sdkResolvedService{{ServiceID: id, ServiceName: "Stripe", PublicTarget: "stripe"}}
	for _, auth := range []*sdkAppAuthDoc{nil, {Type: "basic"}, {Type: "basic", Name: "basicAuth"}} {
		service := sdkConfigServiceDoc{Version: "v1", Auth: auth}
		previous := sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{"Stripe": service}}
		state := sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{"Stripe": service}}
		before, _ := json.Marshal(state)
		summary := []map[string]any{{"name": "Stripe"}}
		pinAppAuthReview(&state, previous, services, []models.SDKSelection{{ServiceID: id, AuthType: "basic", AuthName: "basicAuth"}}, summary)
		after, _ := json.Marshal(state)
		// The resolved review can be richer without changing the persisted config's canonical bytes.
		if string(before) != string(after) || summary[0]["auth"].(*sdkAppAuthDoc).Name != "basicAuth" {
			t.Fatalf("before=%s after=%s summary=%#v", before, after, summary)
		}
		// A credential rotation or new scheme cannot turn a published version into an automatic choice.
		if canInferAppAuth(sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{"stripe": service}}, previous, services[0]) {
			t.Fatal("published auth was eligible for inference")
		}
	}
}

// TestAvailableAuthMustCoverEveryOperation rejects a stored scheme that cannot authorize all selected secured operations.
func TestAvailableAuthMustCoverEveryOperation(t *testing.T) {
	id, contract := availableAuthFixture()
	contract.Operations = append(contract.Operations, securedOperation("basicOnly", "basicAuth"))
	candidates, err := availableAppAuthCandidates(models.SDKSelection{ServiceID: id}, contract)
	// Bearer remains inadmissible even if its credential exists, since the second operation only supports Basic.
	if err != nil || len(candidates) != 1 || candidates[0].AuthName != "basicAuth" {
		t.Fatalf("candidates=%#v error=%v", candidates, err)
	}
}
