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

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

// ─── workspaceTestStore additions for kind: webhook ────────────────────────
func (s *workspaceTestStore) WorkspaceWebhookOwnersByLabel(ctx context.Context, serviceIDs []uuid.UUID, label string) (map[uuid.UUID]string, error) {
	if s.webhookOwnersErr != nil {
		return nil, s.webhookOwnersErr
	}
	if s.webhookOwnersByLabel == nil {
		return map[uuid.UUID]string{}, nil
	}
	return s.webhookOwnersByLabel, nil
}

// ─── validateWebhookConfigDocument (pure) ──────────────────────────────────

func TestValidateWebhookConfigDocument_RequiresName(t *testing.T) {
	err := validateWebhookConfigDocument(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook",
		Services: map[string]webhookConfigServiceDoc{"github": {}},
	})
	if err == nil || !strings.Contains(err.Error(), "requires a name") {
		t.Fatalf("expected missing-name error, got %v", err)
	}
}

func TestValidateWebhookConfigDocument_RequiresAtLeastOneService(t *testing.T) {
	err := validateWebhookConfigDocument(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
	})
	if err == nil || !strings.Contains(err.Error(), "at least one service") {
		t.Fatalf("expected empty-services error, got %v", err)
	}
}

func TestValidateWebhookConfigDocument_RejectsInvalidSecretRef(t *testing.T) {
	err := validateWebhookConfigDocument(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{"github": {Secret: "not-a-valid-ref"}},
	})
	if err == nil || !strings.Contains(err.Error(), "secret reference is invalid") {
		t.Fatalf("expected invalid secret ref error, got %v", err)
	}
}

func TestValidateWebhookConfigDocumentRejectsNonSecretBucketReference(t *testing.T) {
	err := validateWebhookConfigDocument(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{"github": {Secret: "${bucket.prod.env.signing}"}},
	})
	if err == nil || !strings.Contains(err.Error(), "secret reference is invalid") {
		t.Fatalf("expected non-secret reference rejection, got %v", err)
	}
}

func TestWebhookConfigPlanHandlerNeverEchoesCredentialShapedSecret(t *testing.T) {
	credential := "test-provider-credential-material"
	s := &workspaceTestStore{accountID: uuid.New()}
	r := newControlTestRouter(s.accountID)
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(&mockConfigStore{}, s, nil, nil))
	body := []byte(`{"source_hash":"abc","config_key":"webhook:safe","config":{"apiVersion":"fused/v1","kind":"webhook","name":"safe","services":{"github":{"secret":"` + credential + `"}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), credential) {
		t.Fatalf("response echoed credential-shaped secret: %s", rr.Body.String())
	}
}

func TestValidateWebhookConfigDocument_AllowsMissingSecret(t *testing.T) {
	// A provider that doesn't sign webhooks has no secret to configure --
	// see fused-webhook skill's doc comment on services.<slug>.secret.
	err := validateWebhookConfigDocument(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{"github": {}},
	})
	if err != nil {
		t.Fatalf("expected no error for a service with no secret, got %v", err)
	}
}

func TestValidateWebhookConfigDocument_RejectsWrongKind(t *testing.T) {
	err := validateWebhookConfigDocument(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "sdk", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{"github": {}},
	})
	if err == nil || !strings.Contains(err.Error(), "kind webhook") {
		t.Fatalf("expected wrong-kind error, got %v", err)
	}
}

// webhookConfigKey has no version segment, unlike SDK/MCP's artifactConfigKey
// -- see its doc comment for why (a continuously-reconciled bundle, not an
// immutable release).
func TestWebhookConfigKeyHasNoVersionSegment(t *testing.T) {
	if got, want := webhookConfigKey("team-x"), "webhook:team-x"; got != want {
		t.Fatalf("webhookConfigKey(%q) = %q, want %q", "team-x", got, want)
	}
}

func TestResolveWebhookServicesRejectsDuplicateResolvedServiceIdentity(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	s := &workspaceTestStore{
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "GitHub", ServiceSlug: "github"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}},
		},
	}
	_, err := resolveWebhookServices(context.Background(), s, nil, "", webhookConfigDocument{Services: map[string]webhookConfigServiceDoc{
		"GitHub": {}, "github": {},
	}})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusBadRequest || !strings.Contains(httpErr.message, "same activated service") {
		t.Fatalf("duplicate alias resolution error = %#v, want 400 duplicate identity", err)
	}
}

func TestResolveWebhookAuthShapesUsesOneMetadataBatch(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	verifier := &mockVerifier{}
	resolved := map[string]webhookResolvedService{
		"first":  {ServiceID: first, ServiceVersionID: uuid.New(), Version: "v1"},
		"second": {ServiceID: second, ServiceVersionID: uuid.New(), Version: "v2"},
	}
	shapes, err := resolveWebhookAuthShapes(context.Background(), &workspaceTestStore{}, verifier, resolved, []string{"first", "second"})
	if err != nil {
		t.Fatalf("resolveWebhookAuthShapes: %v", err)
	}
	if len(shapes) != 2 || verifier.fetchMetadataCalls != 1 {
		t.Fatalf("shapes=%d metadata batches=%d, want 2 shapes in one batch", len(shapes), verifier.fetchMetadataCalls)
	}
}

func TestPrepareWebhookRegistrationsResolvesAllBucketsInOneBatch(t *testing.T) {
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	s := &workspaceTestStore{bucketsByName: map[string]*store.Bucket{
		"alpha": {ID: uuid.New(), Name: "alpha"},
		"beta":  {ID: uuid.New(), Name: "beta"},
	}}
	resolved := map[string]webhookResolvedService{
		"first":  {ServiceID: first, ServiceVersionID: uuid.New(), Version: "v1", Secret: "${bucket.alpha.secret.one}"},
		"second": {ServiceID: second, ServiceVersionID: uuid.New(), Version: "v1", Secret: "${bucket.beta.secret.two}"},
		"third":  {ServiceID: third, ServiceVersionID: uuid.New(), Version: "v1", Secret: "${bucket.alpha.secret.three}"},
	}
	required, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: s.bucketsByName["alpha"].ID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: s.bucketsByName["beta"].ID}},
	})
	registrations, _, err := prepareWebhookRegistrations(context.Background(), s, &mockVerifier{}, "webhook:batch", required, webhookConfigDocument{Name: "batch"}, resolved, []string{"first", "second", "third"})
	if err != nil {
		t.Fatalf("prepareWebhookRegistrations: %v", err)
	}
	if len(registrations) != 3 || len(s.bucketBatchLookupNames) != 1 || len(s.bucketLookupNames) != 0 {
		t.Fatalf("registrations=%d batch lookups=%#v single lookups=%#v", len(registrations), s.bucketBatchLookupNames, s.bucketLookupNames)
	}
	if got := strings.Join(s.bucketBatchLookupNames[0], ","); got != "alpha,beta" {
		t.Fatalf("batched bucket names = %q, want alpha,beta", got)
	}
	if registrations[0].SecretBucketID == nil || registrations[1].SecretBucketID == nil {
		t.Fatalf("registrations did not persist immutable bucket bindings: %#v", registrations)
	}
}

func TestResolveWebhookSecretRefsRejectsInvalidBeforeBucketQuery(t *testing.T) {
	s := &workspaceTestStore{}
	credential := "test-provider-credential-material"
	_, _, err := resolveWebhookSecretBindings(context.Background(), s, "invalid", map[string]webhookResolvedService{
		"github": {Secret: credential},
	}, []string{"github"})
	if err == nil || len(s.bucketBatchLookupNames) != 0 || !strings.Contains(err.Error(), "webhook secret") {
		t.Fatalf("error=%v batch lookups=%#v, want validation before query", err, s.bucketBatchLookupNames)
	}
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("error echoed credential-shaped secret: %v", err)
	}
}

func TestResolveWebhookSecretRefsRejectsMissingBucketFromBatch(t *testing.T) {
	s := &workspaceTestStore{bucketsByName: map[string]*store.Bucket{"known": {ID: uuid.New(), Name: "known"}}}
	_, _, err := resolveWebhookSecretBindings(context.Background(), s, "missing", map[string]webhookResolvedService{
		"github": {Secret: "${bucket.missing.secret.signing}"},
	}, []string{"github"})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusBadRequest || !strings.Contains(httpErr.message, "bucket not found: missing") {
		t.Fatalf("missing bucket error = %#v", err)
	}
	if len(s.bucketBatchLookupNames) != 1 || len(s.bucketLookupNames) != 0 {
		t.Fatalf("batch lookups=%#v single lookups=%#v", s.bucketBatchLookupNames, s.bucketLookupNames)
	}
}

func TestResolveWebhookSecretRefsClassifiesBucketStoreFailureAsInternal(t *testing.T) {
	s := &workspaceTestStore{bucketBatchLookupErr: errors.New("postgres unavailable")}
	_, _, err := resolveWebhookSecretBindings(context.Background(), s, "store-error", map[string]webhookResolvedService{
		"github": {Secret: "${bucket.production.secret.signing}"},
	}, []string{"github"})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusInternalServerError || httpErr.message != "failed to resolve webhook secret buckets" {
		t.Fatalf("bucket store error = %#v, want generic 500", err)
	}
	if strings.Contains(err.Error(), "production") || strings.Contains(err.Error(), "postgres unavailable") {
		t.Fatalf("bucket store error leaked details: %v", err)
	}
}

type failingWebhookPolicyBatchStore struct {
	workspaceTestStore
}

func (s *failingWebhookPolicyBatchStore) GetEffectiveWorkspaceExecutionPolicyOverrides(context.Context, []store.WorkspaceExecutionPolicyRef) (map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride, error) {
	return nil, errors.New("policy database unavailable")
}

func TestExecuteWebhookConfigApplyFailsClosedOnPolicyLookupError(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	desiredState, _ := json.Marshal(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "fail-closed",
		Services: map[string]webhookConfigServiceDoc{"github": {}},
	})
	s := &failingWebhookPolicyBatchStore{workspaceTestStore: workspaceTestStore{
		accountID:         uuid.New(),
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github", Version: "v1"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
	}}
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: "webhook:fail-closed", ConfigType: store.ConfigTypeWebhook,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState, Revision: 1,
	}}
	_, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, planRevision: 1, sourceHash: "abc",
	})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusInternalServerError || httpErr.message != "failed to resolve webhook execution policy" {
		t.Fatalf("policy lookup error = %#v, want generic 500", err)
	}
	if configStore.webhookApply != nil || configStore.markApplied {
		t.Fatalf("policy lookup failure reached persistence: apply=%#v marked=%v", configStore.webhookApply, configStore.markApplied)
	}
}

// ─── ensureWebhookNameAvailable (uniqueness enforcement) ───────────────────

func TestEnsureWebhookNameAvailable_AllowsWhenUnclaimed(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]string{}}
	err := ensureWebhookNameAvailable(context.Background(), s, "webhook:team-x", "team-x", map[string]webhookResolvedService{
		"github": {ServiceID: serviceID},
	})
	if err != nil {
		t.Fatalf("expected no conflict for an unclaimed (service, name) pair, got %v", err)
	}
}

func TestEnsureWebhookNameAvailable_AllowsReapplyBySameArtifact(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]string{serviceID: "webhook:team-x"}}
	err := ensureWebhookNameAvailable(context.Background(), s, "webhook:team-x", "team-x", map[string]webhookResolvedService{
		"github": {ServiceID: serviceID},
	})
	if err != nil {
		t.Fatalf("expected no conflict when the same artifact re-applies, got %v", err)
	}
}

func TestEnsureWebhookNameAvailable_RejectsOtherArtifactOwnedRegistration(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]string{serviceID: "webhook:team-y"}}
	err := ensureWebhookNameAvailable(context.Background(), s, "webhook:team-x", "team-x", map[string]webhookResolvedService{
		"github": {ServiceID: serviceID},
	})
	if err == nil || !strings.Contains(err.Error(), "webhook:team-y") {
		t.Fatalf("expected conflict naming the other owning config_key, got %v", err)
	}
}

// ─── WebhookConfigPlanHandler (HTTP) ───────────────────────────────────────

func TestWebhookConfigPlanHandler_CreatesPlanForValidConfig(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	bucketID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID: serviceID, ServiceName: "github", Version: "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: "2026-07-01"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
		bucketsByName:        map[string]*store.Bucket{"default": {ID: bucketID, Name: "default"}},
	}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team_id":"00000000-0000-0000-0000-000000000001",
		"config_key": "webhook:team-x",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "webhook",
			"name": "team-x",
			"services": {
				"github": {"secret": "${bucket.default.secret.github_signing}"}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if configStore.createdPlan == nil {
		t.Fatal("expected a plan to be created")
	}
	if configStore.createdPlan.ConfigType != store.ConfigTypeWebhook {
		t.Fatalf("expected ConfigTypeWebhook, got %q", configStore.createdPlan.ConfigType)
	}
	var doc webhookConfigDocument
	if err := json.Unmarshal(configStore.createdPlan.DesiredState, &doc); err != nil {
		t.Fatalf("decode desired state: %v", err)
	}
	if doc.Name != "team-x" {
		t.Fatalf("expected desired state to preserve artifact name, got %#v", doc)
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
	if !hasRequiredPermission(response.RequiredPermissions, "bucket.use", "bucket", bucketID) {
		t.Fatalf("expected bucket.use preview, got %#v", response.RequiredPermissions)
	}
	if len(s.bucketBatchLookupNames) != 1 || len(s.bucketLookupNames) != 0 {
		t.Fatalf("plan bucket lookups = batch %#v point %#v", s.bucketBatchLookupNames, s.bucketLookupNames)
	}
}

type webhookOwnershipDenyStore struct {
	*workspaceTestStore
	preflight store.ArtifactOwnershipPreflight
}

func (s *webhookOwnershipDenyStore) PreflightArtifactOwnership(_ context.Context, input store.ArtifactOwnershipPreflight) (store.ArtifactOwnershipDecision, error) {
	s.preflight = input
	return store.ArtifactOwnershipDecision{
		MembershipAllowed: true,
		TeamMissing: []accesscontrol.Requirement{{
			Permission: accesscontrol.PermissionBucketUse,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: s.workspaceTestStore.bucketsByName["production"].ID},
		}},
	}, nil
}

func TestWebhookConfigPlanHandlerRequiresOwnerTeamBucketUseBeforePlan(t *testing.T) {
	serviceID, bucketID := uuid.New(), uuid.New()
	base := &workspaceTestStore{
		accountID: uuid.New(), workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github", Version: "v1"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: uuid.New(), Version: "v1"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
		bucketsByName:        map[string]*store.Bucket{"production": {ID: bucketID, Name: "production"}},
	}
	s := &webhookOwnershipDenyStore{workspaceTestStore: base}
	configStore := &mockConfigStore{}
	router := newControlTestRouter(base.accountID)
	router.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))
	body := `{"source_hash":"abc","owner_team_id":"00000000-0000-0000-0000-000000000001","config_key":"webhook:denied","config":{"apiVersion":"fused/v1","kind":"webhook","name":"denied","services":{"github":{"secret":"${bucket.production.secret.signing}"}}}}`
	request := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", strings.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || configStore.createdPlan != nil {
		t.Fatalf("status/plan = %d/%#v: %s", response.Code, configStore.createdPlan, response.Body.String())
	}
	foundBucketUse := false
	for _, requirement := range s.preflight.Requirements {
		if requirement.Permission == accesscontrol.PermissionBucketUse && requirement.Resource.ID == bucketID {
			foundBucketUse = true
		}
	}
	if !foundBucketUse {
		t.Fatalf("owner-team preflight requirements = %#v", s.preflight.Requirements)
	}
}

func TestWebhookConfigPlanHandler_RejectsUnactivatedService(t *testing.T) {
	s := &workspaceTestStore{
		accountID:            uuid.New(),
		workspaceID:          uuid.New(),
		webhookOwnersByLabel: map[uuid.UUID]string{},
	}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team_id":"00000000-0000-0000-0000-000000000001",
		"config_key": "webhook:team-x",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "webhook",
			"name": "team-x",
			"services": {"github": {}}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unactivated service, got %d: %s", rr.Code, rr.Body.String())
	}
	if configStore.createdPlan != nil {
		t.Fatal("expected no plan to be created when a referenced service isn't activated")
	}
}

func TestWebhookConfigPlanHandlerRejectsMissingSecretBucketWithoutPlan(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{
		accountID: uuid.New(), workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github", Version: "v1"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: uuid.New(), Version: "v1"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
		bucketsByName:        map[string]*store.Bucket{},
	}
	configStore := &mockConfigStore{}
	router := newControlTestRouter(s.accountID)
	router.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))
	body := `{"source_hash":"abc","owner_team_id":"00000000-0000-0000-0000-000000000001","config_key":"webhook:missing","config":{"apiVersion":"fused/v1","kind":"webhook","name":"missing","services":{"github":{"secret":"${bucket.absent.secret.signing}"}}}}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", strings.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || configStore.createdPlan != nil {
		t.Fatalf("status/plan = %d/%#v: %s", response.Code, configStore.createdPlan, response.Body.String())
	}
	if len(s.bucketBatchLookupNames) != 1 || len(s.bucketLookupNames) != 0 {
		t.Fatalf("bucket lookups = batch %#v point %#v", s.bucketBatchLookupNames, s.bucketLookupNames)
	}
}

func TestWebhookConfigPlanHandler_RejectsNameConflictWithOtherArtifact(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID: serviceID, ServiceName: "github", Version: "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, Version: "2026-07-01"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{serviceID: "webhook:team-y"},
	}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))

	body := []byte(`{
		"source_hash": "abc",
		"owner_team_id":"00000000-0000-0000-0000-000000000001",
		"config_key": "webhook:team-x",
		"config": {
			"apiVersion": "fused/v1",
			"kind": "webhook",
			"name": "team-x",
			"services": {"github": {}}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a name already owned by another artifact, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── executeWebhookConfigApply (upsert + prune) ────────────────────────────

func TestExecuteWebhookConfigApply_UpsertsServicesAndPrunesRemoved(t *testing.T) {
	githubID, jiraID := uuid.New(), uuid.New()
	githubVersionID, jiraVersionID := uuid.New(), uuid.New()
	doc := webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{
			"github": {Secret: "${bucket.default.secret.github_signing}"},
		},
	}
	desiredState, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal desired state: %v", err)
	}
	configKey := "webhook:team-x"
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: githubID, ServiceName: "github", Version: "2026-07-01"},
			{ServiceID: jiraID, ServiceName: "jira", Version: "2026-07-01"},
		},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			githubID: {{ServiceID: githubID, ServiceVersionID: githubVersionID, Version: "2026-07-01"}},
			jiraID:   {{ServiceID: jiraID, ServiceVersionID: jiraVersionID, Version: "2026-07-01"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
		bucketsByName:        map[string]*store.Bucket{"default": {ID: uuid.New(), Name: "default"}},
	}
	required, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: githubID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: s.bucketsByName["default"].ID}},
	})
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: configKey, ConfigType: store.ConfigTypeWebhook,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState,
		RequiredPermissions: required,
	}}

	result, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, planRevision: 1, sourceHash: "abc",
	})
	if err != nil {
		t.Fatalf("executeWebhookConfigApply: %v", err)
	}
	if result.Name != "team-x" || len(result.Applied) != 1 || result.Applied[0].ServiceKey != "github" {
		t.Fatalf("unexpected apply result: %#v", result)
	}
	if configStore.webhookApply == nil || len(configStore.webhookApply.Registrations) != 1 {
		t.Fatalf("expected exactly one atomic webhook apply, got %#v", configStore.webhookApply)
	}
	upserted := configStore.webhookApply.Registrations[0]
	if upserted.Label != "team-x" {
		t.Fatalf("expected upserted row's Label to be the artifact name, got %q", upserted.Label)
	}
	if upserted.OwningConfigKey != configKey {
		t.Fatalf("expected upserted row owned by %q, got %#v", configKey, upserted.OwningConfigKey)
	}
	if upserted.SecretBucketID == nil || *upserted.SecretBucketID != s.bucketsByName["default"].ID {
		t.Fatalf("immutable secret bucket = %#v", upserted.SecretBucketID)
	}
	// jira was in the workspace's activated services but is not declared in
	// doc.Services, so atomic reconciliation must keep only github.
	if len(configStore.webhookApply.KeepServiceIDs) != 1 || configStore.webhookApply.KeepServiceIDs[0] != githubID {
		t.Fatalf("expected atomic prune to keep only github's service id, got %#v", configStore.webhookApply.KeepServiceIDs)
	}
	if !configStore.markApplied {
		t.Fatal("expected the config plan to be marked applied")
	}
	if configStore.upserted == nil || configStore.upserted.OwnerTeamID == nil || *configStore.upserted.OwnerTeamID != *configStore.plan.OwnerTeamID {
		t.Fatalf("webhook config state owner = %#v, want plan owner %v", configStore.upserted, configStore.plan.OwnerTeamID)
	}
}

func TestExecuteWebhookConfigApply_RejectsNameConflictAtApplyTime(t *testing.T) {
	// Defense-in-depth: a second kind: webhook artifact could claim the same
	// (service, name) pair between this artifact's plan and apply -- apply
	// re-checks rather than trusting the plan's already-stale answer.
	githubID := uuid.New()
	doc := webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{"github": {}},
	}
	desiredState, _ := json.Marshal(doc)
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: githubID, ServiceName: "github", Version: "2026-07-01"},
		},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			githubID: {{ServiceID: githubID, Version: "2026-07-01"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{githubID: "webhook:team-y"},
	}
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: "webhook:team-x", ConfigType: store.ConfigTypeWebhook,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState,
	}}

	_, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, planRevision: 1, sourceHash: "abc",
	})
	if err == nil {
		t.Fatal("expected apply to reject a name now owned by another artifact")
	}
	if configStore.webhookApply != nil {
		t.Fatalf("rejected apply reached atomic persistence: %#v", configStore.webhookApply)
	}
}

func TestExecuteWebhookConfigApplyRejectsDeleteRecreateBucketSubstitution(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	originalBucketID, replacementBucketID := uuid.New(), uuid.New()
	desiredState, _ := json.Marshal(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "immutable",
		Services: map[string]webhookConfigServiceDoc{"github": {Secret: "${bucket.production.secret.signing}"}},
	})
	required, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: originalBucketID}},
	})
	s := &workspaceTestStore{
		accountID: uuid.New(), workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github", Version: "v1"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
		bucketsByName:        map[string]*store.Bucket{"production": {ID: replacementBucketID, Name: "production"}},
	}
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: "webhook:immutable", ConfigType: store.ConfigTypeWebhook,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState,
		RequiredPermissions: required, Revision: 1,
	}}
	verifier := &mockVerifier{}
	_, err := executeWebhookConfigApply(context.Background(), configStore, s, verifier, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, planRevision: 1, sourceHash: "abc",
	})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusConflict {
		t.Fatalf("replacement binding error = %#v", err)
	}
	if configStore.webhookApply != nil || configStore.markApplied || verifier.fetchMetadataCalls != 0 {
		t.Fatalf("stale binding side effects = apply %#v marked %v metadata %d", configStore.webhookApply, configStore.markApplied, verifier.fetchMetadataCalls)
	}
}

func TestExecuteWebhookConfigApply_AtomicRepositoryFailureLeavesNoStoreWrites(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	desiredState, _ := json.Marshal(webhookConfigDocument{
		APIVersion: "fused/v1", Kind: "webhook", Name: "team-x",
		Services: map[string]webhookConfigServiceDoc{"github": {}},
	})
	s := &workspaceTestStore{
		accountID: uuid.New(), workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github", Version: "2026-07-01"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "2026-07-01"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]string{},
	}
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID: uuid.New(), ConfigKey: "webhook:team-x", ConfigType: store.ConfigTypeWebhook,
			Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState, Revision: 1,
		},
		webhookErr: errors.New("commit failed"),
	}

	_, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, planRevision: 1, sourceHash: "abc",
	})
	if err == nil {
		t.Fatal("expected atomic repository failure")
	}
	if configStore.markApplied {
		t.Fatal("atomic repository failure marked the plan applied")
	}
	configStore.webhookErr = nil
	result, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, planRevision: 1, sourceHash: "abc",
	})
	if err != nil || len(result.Applied) != 1 || !configStore.markApplied {
		t.Fatalf("retry after atomic rollback = %#v, %v, marked=%v", result, err, configStore.markApplied)
	}
}

// ─── webhookConfigApplyResponse (pure wire-shape mapping) ──────────────────

func TestWebhookConfigApplyResponse_ListsEveryRegistration(t *testing.T) {
	planID := uuid.New()
	resp := webhookConfigApplyResponse(planID, webhookConfigApplyResult{
		ConfigKey: "webhook:team-x", Name: "team-x",
		Applied: []appliedWorkspaceWebhook{
			{ServiceKey: "github", Slug: "slug-a"},
			{ServiceKey: "jira", Slug: "slug-b"},
		},
	})
	registrations, ok := resp["registrations"].([]map[string]any)
	if !ok || len(registrations) != 2 {
		t.Fatalf("expected two registrations in response, got %#v", resp["registrations"])
	}
	if resp["name"] != "team-x" {
		t.Fatalf("expected name in response, got %#v", resp["name"])
	}
	// No runtime/package artifact exists for kind: webhook -- unlike SDK/MCP,
	// the response must never claim one.
	for _, unexpected := range []string{"artifact_id", "artifact_id", "mcp_url", "execution_token"} {
		if _, exists := resp[unexpected]; exists {
			t.Fatalf("webhook apply response must not carry %q", unexpected)
		}
	}
}
