package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type webhookPageTestStore struct {
	*workspaceTestStore
	calls         int
	scope         accesscontrol.AuthorizedScope
	filter        store.WorkspaceWebhookFilter
	limit, offset int
	bounded       bool
	rows          []store.WorkspaceWebhookListing
}

// ListAuthorizedWorkspaceWebhookPage records the only persistence call needed by the catalogue resolver.
func (s *webhookPageTestStore) ListAuthorizedWorkspaceWebhookPage(ctx context.Context, scope accesscontrol.AuthorizedScope, filter store.WorkspaceWebhookFilter, limit, offset int) ([]store.WorkspaceWebhookListing, int, error) {
	s.calls++
	s.scope = scope
	s.filter = filter
	s.limit = limit
	s.offset = offset
	_, s.bounded = ctx.Deadline()
	return s.rows, len(s.rows), nil
}

// TestWorkspaceWebhookPageGraphQL preserves resource authorization and secret redaction without Registry access.
func TestWorkspaceWebhookPageGraphQL(t *testing.T) {
	account, workspace, service, bucket := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repository := &webhookPageTestStore{workspaceTestStore: &workspaceTestStore{accountID: account, workspaceID: workspace}, rows: []store.WorkspaceWebhookListing{{WorkspaceWebhook: store.WorkspaceWebhook{ServiceID: service, Label: "Events", Slug: "route", CallbackURL: "https://engine.example/webhook/route", SecretBucketID: &bucket, SecretRef: "${bucket.private.secret.hidden}"}, ServiceName: "Payments", ServiceRef: "@stripe/payments"}}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, repository, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"), nil, nil)
	require.NoError(t, err)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithResourcePermissions(t, workspace, accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service}})
	actor.AccountID = account
	body := `{"query":"query { workspaceWebhookPage(limit: 10, offset: 2, search: \" Events \", service_id: \"` + service.String() + `\") { total items { service_id service_name service_ref slug callback_url signature signing_secret { bucket_id key_name } } } }"}`
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), `"errors"`)
	require.Contains(t, response.Body.String(), "@stripe/payments")
	require.NotContains(t, response.Body.String(), "private")
	require.Contains(t, response.Body.String(), `"signing_secret":null`)
	require.Equal(t, 1, repository.calls)
	require.False(t, repository.scope.All)
	require.Equal(t, []uuid.UUID{service}, repository.scope.IDs)
	require.Equal(t, service, *repository.filter.ServiceID)
	require.Equal(t, "Events", repository.filter.Search)
	require.Equal(t, 10, repository.limit)
	require.Equal(t, 2, repository.offset)
	require.True(t, repository.bounded)
	// A user without service read authority cannot reach storage through the new collection root.
	actor = actorWithResourcePermissions(t, workspace)
	actor.AccountID = account
	request = httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response = httptest.NewRecorder()
	handler(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Equal(t, 1, repository.calls)
}
