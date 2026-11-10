package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type webhookDiagnosticRouteStore struct {
	store.Store
	scope store.WebhookDiagnosticScope
	reads int
}

// webhookDiagnosticTestGrants builds only the independently requested grants without adding inherited permissions.
func webhookDiagnosticTestGrants(actor accesscontrol.Actor, serviceID uuid.UUID, service, audit, diagnostic bool, diagnosticServiceID uuid.UUID) []accesscontrol.Grant {
	grants := []accesscontrol.Grant{}
	for _, item := range []struct {
		enabled    bool
		permission accesscontrol.Permission
		resource   accesscontrol.ResourceRef
	}{
		{service, accesscontrol.PermissionServiceRead, accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		{audit, accesscontrol.PermissionAuditRead, accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}},
		{diagnostic, accesscontrol.PermissionWebhookDiagnosticsRead, accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: diagnosticServiceID}},
	} {
		// A disabled matrix capability must never enter the actor's snapshot.
		if item.enabled {
			grants = append(grants, accesscontrol.Grant{Permission: item.permission, Resource: item.resource})
		}
	}
	return grants
}

// SaveWebhookDiagnostics is unused by this read-boundary fixture.
func (s *webhookDiagnosticRouteStore) SaveWebhookDiagnostics(context.Context, store.WebhookDiagnosticScope, json.RawMessage, []byte) error {
	return nil
}

// DeleteExpiredWebhookDiagnostics is unused by this read-boundary fixture.
func (s *webhookDiagnosticRouteStore) DeleteExpiredWebhookDiagnostics(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// GetWebhookDiagnostics tracks whether denied actors reached private storage and preserves exact scope.
func (s *webhookDiagnosticRouteStore) GetWebhookDiagnostics(_ context.Context, scope store.WebhookDiagnosticScope, _ []byte) (json.RawMessage, error) {
	s.reads++
	// Even authorized service readers cannot substitute a different account, receipt or registration.
	if scope != s.scope {
		return nil, store.ErrExecutionResultNotFound
	}
	return json.RawMessage(`{"body":"PRIVATE_SENTINEL","encoding":"utf-8"}`), nil
}

// TestWebhookDiagnosticsAuthorization tests every permission independently and rejects runtime and cross-scope access.
func TestWebhookDiagnosticsAuthorization(t *testing.T) {
	scope := store.WebhookDiagnosticScope{AccountID: uuid.New(), ServiceID: uuid.New(), RegistrationID: uuid.New(), EventID: uuid.New()}
	for _, tc := range []struct {
		name                                                                                     string
		service, audit, diagnostic, foreignService, foreignAccount, runtime, foreignRegistration bool
		status                                                                                   int
	}{
		{name: "no grants", status: 403},
		{name: "ordinary receipt", service: true, audit: true, status: 403},
		{name: "diagnostic alone", diagnostic: true, status: 403},
		{name: "no audit", service: true, diagnostic: true, status: 403},
		{name: "no service read", audit: true, diagnostic: true, status: 403},
		{name: "explicit exact grant", service: true, audit: true, diagnostic: true, status: 200},
		{name: "other service grant", service: true, audit: true, diagnostic: true, foreignService: true, status: 403},
		{name: "other account", service: true, audit: true, diagnostic: true, foreignAccount: true, status: 404},
		{name: "other registration", service: true, audit: true, diagnostic: true, foreignRegistration: true, status: 404},
		{name: "runtime app", service: true, audit: true, diagnostic: true, runtime: true, status: 401},
	} {
		// Each table case uses a fresh actor and repository so a prior successful reveal cannot mask a denied read.
		t.Run(tc.name, func(t *testing.T) {
			repository := &webhookDiagnosticRouteStore{scope: scope}
			actor := controlTestOwnerActor(scope.AccountID)
			diagnosticServiceID := scope.ServiceID
			// An unrelated service diagnostic grant must not unlock this receipt.
			if tc.foreignService {
				diagnosticServiceID = uuid.New()
			}
			grants := webhookDiagnosticTestGrants(actor, scope.ServiceID, tc.service, tc.audit, tc.diagnostic, diagnosticServiceID)
			var err error
			actor.Authorization, err = appPermissionTestSnapshot(1, grants...)
			require.NoError(t, err)
			// Valid permissions never override the tenant or management-actor boundary.
			if tc.foreignAccount {
				actor.AccountID = uuid.New()
			}
			if tc.runtime {
				actor.Kind = accesscontrol.SubjectApp
			}
			registration := scope.RegistrationID
			// An opaque receipt ID cannot reveal its body under another registration URL.
			if tc.foreignRegistration {
				registration = uuid.New()
			}
			router := chi.NewRouter()
			router.Get("/workspace/services/{service_id}/webhooks/{registration_id}/receipts/{event_id}/diagnostics", WebhookDiagnosticsHandler(repository, nil))
			request := httptest.NewRequest(http.MethodGet, "/workspace/services/"+scope.ServiceID.String()+"/webhooks/"+registration.String()+"/receipts/"+scope.EventID.String()+"/diagnostics", nil)
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			// A permission denial must occur before decryption, not merely redact the response afterward.
			if tc.status == 403 || tc.status == 401 {
				require.Zero(t, repository.reads)
			}
			if tc.status == 200 {
				require.Contains(t, response.Body.String(), "PRIVATE_SENTINEL")
			} else {
				require.NotContains(t, response.Body.String(), "PRIVATE_SENTINEL")
			}
		})
	}
}
