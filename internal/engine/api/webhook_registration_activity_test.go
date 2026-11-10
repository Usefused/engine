package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/stretchr/testify/require"
)

type webhookActivityTestStore struct {
	*webhookPageTestStore
	filters []store.WebhookActivityFilter
}

// ListWebhookActivity observes the exact SQL scope admitted by the real GraphQL boundary.
func (s *webhookActivityTestStore) ListWebhookActivity(_ context.Context, filter store.WebhookActivityFilter) ([]models.WebhookEvent, int64, error) {
	s.filters = append(s.filters, filter)
	return []models.WebhookEvent{{ID: uuid.New(), EventType: "invoice.paid", DeliveryStatus: "failed", ErrorReason: "delivery_failed"}}, 1, nil
}

// GetWebhookActivityAnalytics shares recorded inputs so tests can compare history and aggregate scope.
func (s *webhookActivityTestStore) GetWebhookActivityAnalytics(_ context.Context, filter store.WebhookActivityFilter) (models.WebhookAnalytics, error) {
	s.filters = append(s.filters, filter)
	return models.WebhookAnalytics{TotalIngested: 1, TotalFailed: 1, AverageLatencyMs: 12, P95LatencyMs: 12}, nil
}

// webhookRegistrationRequest exercises HTTP preflight rather than invoking resolvers with forged context.
func webhookRegistrationRequest(t *testing.T, handler http.Handler, actor accesscontrol.Actor, query string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Host = "untrusted.example"
	request.Header.Set("X-Forwarded-Host", "untrusted.example")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestWebhookLoadedPublicURL covers YAML-only configuration and exact registered URLs through the real projection.
func TestWebhookLoadedPublicURL(t *testing.T) {
	t.Setenv("FUSED_ENGINE_PUBLIC_URL", "")
	account, workspace, service, registration := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repository := &webhookPageTestStore{workspaceTestStore: &workspaceTestStore{accountID: account, workspaceID: workspace}, rows: []store.WorkspaceWebhookListing{{WorkspaceWebhook: store.WorkspaceWebhook{ID: registration, ServiceID: service, Slug: "route", Label: "Events"}, ServiceName: "Payments"}}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, repository, &mockVerifier{}, &mockRegistryClient{}, nil, nil, nil)
	require.NoError(t, err)
	actor := actorWithResourcePermissions(t, workspace, accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service}})
	actor.AccountID = account
	handler := EnginePublicURLMiddleware("https://configured.example/base")(mcpGraphQLHandler(schema))
	query := `query { workspaceWebhook(service_id:"` + service.String() + `", slug:"route") { id callback_url signing_secret { key_name } } }`
	response := webhookRegistrationRequest(t, handler, actor, query)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), `"errors"`)
	require.Contains(t, response.Body.String(), "https://configured.example/base/webhook/route")
	require.NotContains(t, response.Body.String(), "untrusted.example")
	require.Equal(t, "route", repository.filter.Slug)
	require.Equal(t, service, *repository.filter.ServiceID)
	require.Equal(t, 1, repository.limit)
	require.Contains(t, response.Body.String(), registration.String())
	// Exact URLs pinned by signature policies remain immutable when deployment configuration changes.
	repository.rows[0].CallbackURL = "https://pinned.example/webhook/route"
	response = webhookRegistrationRequest(t, handler, actor, query)
	require.Contains(t, response.Body.String(), "https://pinned.example/webhook/route")
	// No authorized match is absence, rather than a fallback search or fabricated URL.
	repository.rows = nil
	response = webhookRegistrationRequest(t, handler, actor, query)
	require.Contains(t, response.Body.String(), `"workspaceWebhook":null`)
	// A direct detail query without service authority must stop before registration storage.
	before := repository.calls
	denied := actorWithResourcePermissions(t, workspace)
	denied.AccountID = account
	response = webhookRegistrationRequest(t, handler, denied, query)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Equal(t, before, repository.calls)

}

// TestWebhookRegistrationActivityAuthorization proves direct HTTP requests cannot bypass service and audit permissions.
func TestWebhookRegistrationActivityAuthorization(t *testing.T) {
	account, workspace, service, registration := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, scenario := range []string{"both", "service-only", "audit-only", "neither", "other-service"} {
		// Each actor has independent call accounting; no previous result can satisfy a denied case.
		t.Run(scenario, func(t *testing.T) {
			repository := &webhookActivityTestStore{webhookPageTestStore: &webhookPageTestStore{workspaceTestStore: &workspaceTestStore{accountID: account, workspaceID: workspace, workspaceServices: []store.WorkspaceService{{ServiceID: service, Version: "1.0.0"}}, workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{service: {{ServiceID: service, Version: "1.0.0"}}}}}}
			serviceGrant := accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service}}
			otherServiceGrant := accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()}}
			auditGrant := accesscontrol.Grant{Permission: accesscontrol.PermissionAuditRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}}
			grants := map[string][]accesscontrol.Grant{"both": {serviceGrant, auditGrant}, "service-only": {serviceGrant}, "audit-only": {auditGrant}, "neither": {}, "other-service": {otherServiceGrant, auditGrant}}[scenario]
			actor := actorWithResourcePermissions(t, workspace, grants...)
			actor.AccountID = account
			schema, err := newMCPGraphQLSchema(&mockConfigStore{}, repository, &mockVerifier{}, &mockRegistryClient{}, nil, nil, nil)
			require.NoError(t, err)
			query := `query { webhookEvents(service_id:"` + service.String() + `",registration_id:"` + registration.String() + `",status:"failed",limit:20,offset:40,start_date:"2026-11-01T00:00:00Z") { total items { error_reason } } webhookAnalytics(service_id:"` + service.String() + `",registration_id:"` + registration.String() + `",start_date:"2026-11-01T00:00:00Z") { total_ingested total_failed average_latency_ms } }`
			response := webhookRegistrationRequest(t, mcpGraphQLHandler(schema), actor, query)
			// Authorization runs before either resolver, including aliases and compound queries.
			if scenario != "both" {
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
				require.Empty(t, repository.filters)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.NotContains(t, response.Body.String(), `"errors"`)
			require.Len(t, repository.filters, 2)
			// Both independent reporting paths retain the authenticated account and exact registration.
			for _, filter := range repository.filters {
				require.Equal(t, account, filter.AccountID)
				require.Equal(t, service, filter.ServiceID)
				require.Equal(t, registration, *filter.RegistrationID)
				require.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), *filter.StartDate)
			}
			require.Contains(t, response.Body.String(), "delivery_failed")
		})
	}
}

// TestWebhookRegistrationFilterValidation prevents malformed identities and dates from widening activity reads.
func TestWebhookRegistrationFilterValidation(t *testing.T) {
	for _, args := range []map[string]interface{}{
		{"registration_id": "bad"}, {"status": "arbitrary"}, {"start_date": "bad"},
		{"end_date": "bad"}, {"start_date": "2026-11-02T00:00:00Z", "end_date": "2026-11-01T00:00:00Z"},
	} {
		args["service_id"] = uuid.NewString()
		_, err := webhookAnalyticsFilterFromArgs(graphql.ResolveParams{Args: args})
		require.Error(t, err)
	}
}
