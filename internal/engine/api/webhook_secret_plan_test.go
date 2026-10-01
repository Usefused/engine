package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type webhookSecretTestStore struct {
	*workspaceTestStore
	registration store.WorkspaceWebhook
}

// GetWorkspaceWebhookBySlug provides one existing ingress identity without changing any runtime state.
func (s *webhookSecretTestStore) GetWorkspaceWebhookBySlug(context.Context, string) (*store.WorkspaceWebhook, error) {
	return &s.registration, nil
}

// TestWebhookSecretPlanRetainsBundle exercises authorization and the actual plan builder rather than a UI-only permission check.
func TestWebhookSecretPlanRetainsBundle(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	defer entitlement.LiveEntitlement.Reset()
	for _, scenario := range []string{"allowed", "reader", "wrong-account", "bucket-denied", "stale"} {
		// Each case owns independent fixtures so a rejected request cannot inherit a previous receipt.
		t.Run(scenario, func(t *testing.T) {
			accountID, serviceID, otherID, bucketID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			actor := controlTestOwnerActor(accountID)
			// Readers must be stopped before loading private registration state.
			if scenario == "reader" {
				actor = registryPolicyActor(t, accesscontrol.PermissionServiceRead)
				actor.AccountID = accountID
			}
			// A manager without bucket.use must not repoint verification to that credential.
			if scenario == "bucket-denied" {
				actor = actorWithResourcePermissions(t, actor.WorkspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionAppWebhookManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}, accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}, accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
				actor.AccountID = accountID
			}
			fixture := &webhookSecretTestStore{workspaceTestStore: &workspaceTestStore{accountID: accountID, workspaceID: actor.WorkspaceID, workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "stripe", Version: "v1"}, {ServiceID: otherID, ServiceName: "github", Version: "v1"}}, workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, ServiceVersionID: uuid.New(), Version: "v1"}}, otherID: {{ServiceID: otherID, ServiceVersionID: uuid.New(), Version: "v1"}}}, webhookOwnersByLabel: map[uuid.UUID]string{serviceID: "webhook:events", otherID: "webhook:events"}, bucketsByName: map[string]*store.Bucket{"default": {ID: bucketID, Name: "default"}}}, registration: store.WorkspaceWebhook{AccountID: accountID, ServiceID: serviceID, OwningConfigKey: "webhook:events", Slug: "stable-url"}}
			// The global ingress lookup must never confer cross-account management authority.
			if scenario == "wrong-account" {
				fixture.registration.AccountID = uuid.New()
			}
			configs := &mockConfigStore{state: &store.ConfigState{ConfigKey: "webhook:events", ConfigType: store.ConfigTypeWebhook, Generation: 2, OwnerSubjectID: &actor.SubjectID, DesiredState: json.RawMessage(`{"apiVersion":"fused/v1","kind":"webhook","name":"events","callback_base_url":"https://engine.example/base","services":{"stripe":{"secret":"${bucket.default.secret.old}"},"github":{"secret":"${bucket.default.secret.unchanged}"}}}`)}}
			// Simulate a competing change between source loading and the normal planning boundary.
			if scenario == "stale" {
				configs.plan = &store.ConfigPlan{BaseGeneration: 3}
			}
			request := httptest.NewRequest(http.MethodPost, "/webhook-config/signing-secret/plan", strings.NewReader(`{"slug":"stable-url","secret":"${bucket.default.secret.new}"}`))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			WebhookSigningSecretPlanHandler(configs, fixture, nil, nil)(response, request)
			// Only the authorized, current case can obtain an applyable review receipt.
			if scenario != "allowed" {
				if response.Code == http.StatusOK {
					t.Fatalf("unauthorized/stale edit accepted: %s", response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK || configs.createdPlan == nil {
				t.Fatalf("plan failed: %d %s", response.Code, response.Body.String())
			}
			var doc webhookConfigDocument
			if err := json.Unmarshal(configs.createdPlan.DesiredState, &doc); err != nil {
				t.Fatal(err)
			}
			// Updating a single card must not prune siblings or change the provider's receiving address.
			if len(doc.Services) != 2 || doc.Services["stripe"].Secret != "${bucket.default.secret.new}" || doc.Services["github"].Secret != "${bucket.default.secret.unchanged}" || doc.CallbackBaseURL != "https://engine.example/base" {
				t.Fatal("secret plan changed unrelated configuration")
			}
			if strings.Contains(response.Body.String(), "bucket.default") || configs.webhookApply != nil {
				t.Fatal("plan leaked a reference or applied without review")
			}
		})
	}
}

// TestWebhookSecretPlanRejectsRawOrEmptySecret proves this edit cannot disable verification or persist raw credentials.
func TestWebhookSecretPlanRejectsRawOrEmptySecret(t *testing.T) {
	for _, secret := range []string{"", "raw-signing-secret", "${bucket.default.value.key}"} {
		doc := webhookConfigDocument{APIVersion: "fused/v1", Kind: "webhook", Name: "events", Services: map[string]webhookConfigServiceDoc{"stripe": {}}}
		if _, err := webhookDocumentWithSigningSecret(doc, "stripe", secret); err == nil {
			t.Fatal("invalid replacement accepted")
		}
	}
}
