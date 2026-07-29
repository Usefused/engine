package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

// ─── workspaceTestStore additions for kind: webhook ────────────────────────
//
// These two methods are kind: webhook's own store surface -- distinct from
// UpsertWorkspaceWebhook/PruneWorkspaceWebhooks (workspace_handlers_test.go),
// which the now-deleted legacy runtime_config.webhooks path used. Defined
// here, next to the tests that exercise them, rather than alongside the
// struct's other webhook fields, since nothing outside webhook_config_handlers
// calls them.

func (s *workspaceTestStore) PruneOwnedWorkspaceWebhooks(ctx context.Context, owningConfigKey string, keepServiceIDs []uuid.UUID) ([]uuid.UUID, error) {
	s.prunedOwnedCalls = append(s.prunedOwnedCalls, prunedOwnedWebhooksCall{owningConfigKey: owningConfigKey, keepServiceIDs: keepServiceIDs})
	if s.pruneOwnedWebhooksErr != nil {
		return nil, s.pruneOwnedWebhooksErr
	}
	return s.pruneOwnedWebhooksResp, nil
}

func (s *workspaceTestStore) WorkspaceWebhookOwnersByLabel(ctx context.Context, serviceIDs []uuid.UUID, label string) (map[uuid.UUID]*string, error) {
	if s.webhookOwnersErr != nil {
		return nil, s.webhookOwnersErr
	}
	if s.webhookOwnersByLabel == nil {
		return map[uuid.UUID]*string{}, nil
	}
	return s.webhookOwnersByLabel, nil
}

func strPtr(s string) *string { return &s }

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
	if err == nil || !strings.Contains(err.Error(), "secret is invalid") {
		t.Fatalf("expected invalid secret ref error, got %v", err)
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

// ─── ensureWebhookNameAvailable (uniqueness enforcement) ───────────────────

func TestEnsureWebhookNameAvailable_AllowsWhenUnclaimed(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]*string{}}
	err := ensureWebhookNameAvailable(context.Background(), s, "webhook:team-x", "team-x", map[string]webhookResolvedService{
		"github": {ServiceID: serviceID},
	})
	if err != nil {
		t.Fatalf("expected no conflict for an unclaimed (service, name) pair, got %v", err)
	}
}

func TestEnsureWebhookNameAvailable_AllowsReapplyBySameArtifact(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]*string{serviceID: strPtr("webhook:team-x")}}
	err := ensureWebhookNameAvailable(context.Background(), s, "webhook:team-x", "team-x", map[string]webhookResolvedService{
		"github": {ServiceID: serviceID},
	})
	if err != nil {
		t.Fatalf("expected no conflict when the same artifact re-applies, got %v", err)
	}
}

func TestEnsureWebhookNameAvailable_RejectsLegacyOwnedRegistration(t *testing.T) {
	serviceID := uuid.New()
	// A nil owner means a legacy runtime_config.webhooks row already claimed
	// this (service, name) pair -- see WorkspaceWebhookOwnersByLabel's doc
	// comment and plans/plan-webhook-kind.md.
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]*string{serviceID: nil}}
	err := ensureWebhookNameAvailable(context.Background(), s, "webhook:team-x", "team-x", map[string]webhookResolvedService{
		"github": {ServiceID: serviceID},
	})
	if err == nil || !strings.Contains(err.Error(), "legacy webhook registration") {
		t.Fatalf("expected legacy-registration conflict, got %v", err)
	}
}

func TestEnsureWebhookNameAvailable_RejectsOtherArtifactOwnedRegistration(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{webhookOwnersByLabel: map[uuid.UUID]*string{serviceID: strPtr("webhook:team-y")}}
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
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{{
			ServiceID: serviceID, ServiceName: "github", Version: "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: "2026-07-01"}},
		},
		webhookOwnersByLabel: map[uuid.UUID]*string{},
	}
	configStore := &mockConfigStore{}

	r := chi.NewRouter()
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))

	body := []byte(`{
		"source_hash": "abc",
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
}

func TestWebhookConfigPlanHandler_RejectsUnactivatedService(t *testing.T) {
	s := &workspaceTestStore{
		accountID:            uuid.New(),
		workspaceID:          uuid.New(),
		webhookOwnersByLabel: map[uuid.UUID]*string{},
	}
	configStore := &mockConfigStore{}

	r := chi.NewRouter()
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))

	body := []byte(`{
		"source_hash": "abc",
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
		webhookOwnersByLabel: map[uuid.UUID]*string{serviceID: strPtr("webhook:team-y")},
	}
	configStore := &mockConfigStore{}

	r := chi.NewRouter()
	r.Post("/webhook-config/plan", WebhookConfigPlanHandler(configStore, s, nil, nil))

	body := []byte(`{
		"source_hash": "abc",
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
		webhookOwnersByLabel: map[uuid.UUID]*string{},
		bucketsByName:        map[string]*store.Bucket{"default": {ID: uuid.New()}},
	}
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: configKey, ConfigType: store.ConfigTypeWebhook,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState,
	}}

	result, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, sourceHash: "abc",
	})
	if err != nil {
		t.Fatalf("executeWebhookConfigApply: %v", err)
	}
	if result.Name != "team-x" || len(result.Applied) != 1 || result.Applied[0].ServiceKey != "github" {
		t.Fatalf("unexpected apply result: %#v", result)
	}
	if len(s.upsertedWebhooks) != 1 {
		t.Fatalf("expected exactly one webhook upsert, got %#v", s.upsertedWebhooks)
	}
	upserted := s.upsertedWebhooks[0]
	if upserted.Label != "team-x" {
		t.Fatalf("expected upserted row's Label to be the artifact name, got %q", upserted.Label)
	}
	if upserted.OwningConfigKey == nil || *upserted.OwningConfigKey != configKey {
		t.Fatalf("expected upserted row owned by %q, got %#v", configKey, upserted.OwningConfigKey)
	}
	// jira was in the workspace's activated services but is not declared in
	// doc.Services -- PruneOwnedWorkspaceWebhooks must be told to keep only
	// github, so a previously-registered jira row (if any) gets removed.
	if len(s.prunedOwnedCalls) != 1 {
		t.Fatalf("expected exactly one prune call, got %#v", s.prunedOwnedCalls)
	}
	pruneCall := s.prunedOwnedCalls[0]
	if pruneCall.owningConfigKey != configKey {
		t.Fatalf("expected prune scoped to %q, got %q", configKey, pruneCall.owningConfigKey)
	}
	if len(pruneCall.keepServiceIDs) != 1 || pruneCall.keepServiceIDs[0] != githubID {
		t.Fatalf("expected prune to keep only github's service id, got %#v", pruneCall.keepServiceIDs)
	}
	if !configStore.markApplied {
		t.Fatal("expected the config plan to be marked applied")
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
		webhookOwnersByLabel: map[uuid.UUID]*string{githubID: strPtr("webhook:team-y")},
	}
	configStore := &mockConfigStore{plan: &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: "webhook:team-x", ConfigType: store.ConfigTypeWebhook,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", DesiredState: desiredState,
	}}

	_, err := executeWebhookConfigApply(context.Background(), configStore, s, &mockVerifier{}, nil, sdkApplyCall{
		apiKey: "fsk_test", accountID: s.accountID, planID: configStore.plan.ID, sourceHash: "abc",
	})
	if err == nil {
		t.Fatal("expected apply to reject a name now owned by another artifact")
	}
	if len(s.upsertedWebhooks) != 0 {
		t.Fatalf("expected no webhook row written on a rejected apply, got %#v", s.upsertedWebhooks)
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
