package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type bucketConsentStore struct {
	*workspaceTestStore
	manifest []byte
}

// OAuthRuntimeManifest supplies the registered request rather than accepting browser-authored routing.
func (s *bucketConsentStore) OAuthRuntimeManifest(context.Context, string) ([]byte, error) {
	return s.manifest, nil
}

// GetConnectBranding keeps consent rendering independent of unrelated settings fixtures.
func (*bucketConsentStore) GetConnectBranding(context.Context) (store.ConnectBranding, error) {
	return store.DefaultConnectBranding(), nil
}

// TestRuntimeCallerBucketRendersReadOnly verifies the registered choice, not a query-string override, controls the page.
func TestRuntimeCallerBucketRendersReadOnly(t *testing.T) {
	id := uuid.New()
	available := []store.BucketSummary{{ID: id, Name: "demo"}}
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionBucketUse)
	// Both human-readable configuration and immutable caller IDs resolve to the same read-only review.
	for _, reference := range []string{"demo", id.String()} {
		raw := strings.Replace(runtimeManifestFixture, `"schema_version":1`, `"schema_version":1,"bucket":"`+reference+`"`, 1)
		source := &bucketConsentStore{workspaceTestStore: &workspaceTestStore{bucketSummaries: available}, manifest: []byte(raw)}
		handler := &runtimeOAuthHandler{store: source}
		response := httptest.NewRecorder()
		handler.renderAuthorization(response, httptest.NewRequest(http.MethodGet, "/oauth/runtime/authorize?bucket=another", nil), actor, oauthprovider.AuthorizeResult{}, oauthprovider.AuthorizeRequest{ClientID: "client"})
		require.Equal(t, 200, response.Code)
		require.Contains(t, response.Body.String(), `<strong>demo</strong>`)
		require.Contains(t, response.Body.String(), `name="bucket" value="`+id.String()+`"`)
		require.NotContains(t, response.Body.String(), `<select`)
		require.NotContains(t, response.Body.String(), `name="runtime_bucket"`)
		require.NotContains(t, response.Body.String(), `role="combobox"`)
	}
	// A missing or unauthorized requested bucket fails closed instead of asking for a substitute.
	for _, reference := range []string{"missing", uuid.NewString()} {
		source := &bucketConsentStore{workspaceTestStore: &workspaceTestStore{bucketSummaries: available}, manifest: []byte(strings.Replace(runtimeManifestFixture, `"schema_version":1`, `"schema_version":1,"bucket":"`+reference+`"`, 1))}
		response := httptest.NewRecorder()
		(&runtimeOAuthHandler{store: source}).renderAuthorization(response, httptest.NewRequest(http.MethodGet, "/oauth/runtime/authorize", nil), actor, oauthprovider.AuthorizeResult{}, oauthprovider.AuthorizeRequest{ClientID: "client"})
		require.Equal(t, 403, response.Code)
		require.NotContains(t, response.Body.String(), `<select`)
	}
}

// TestRuntimeFixedBucketSubmissionRejectsTampering rechecks the choice and live authority before any app publication.
func TestRuntimeFixedBucketSubmissionRejectsTampering(t *testing.T) {
	chosen, other := uuid.New(), uuid.New()
	source := &workspaceTestStore{bucketsByID: map[uuid.UUID]*store.Bucket{chosen: {ID: chosen, Name: "demo"}, other: {ID: other, Name: "default"}}}
	handler := &runtimeOAuthHandler{store: source}
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionBucketUse)
	manifest := runtimeAccessManifest{Bucket: "demo"}
	// Missing, swapped and expanded posted identities cannot override the caller-selected route.
	for _, form := range []url.Values{{"bucket": {chosen.String()}}, {"bucket": {other.String()}}, {"bucket": {chosen.String()}, "runtime_bucket": {other.String()}}, {}} {
		req := httptest.NewRequest(http.MethodPost, "/oauth/runtime/authorize/consent", nil)
		req.PostForm = form
		buckets, err := handler.selectedBuckets(req, actor, manifest)
		// Only the exact requested route can be approved even when the actor can use both buckets.
		if form.Get("bucket") == chosen.String() && len(form["runtime_bucket"]) == 0 {
			require.NoError(t, err)
			require.Equal(t, chosen, buckets[0].ID)
		} else {
			require.Error(t, err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/runtime/authorize/consent", nil)
	req.PostForm = url.Values{"bucket": {chosen.String()}}
	restricted := actorWithResourcePermissions(t, actor.WorkspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: other}})
	_, err := handler.selectedBuckets(req, restricted, manifest)
	require.Error(t, err)
	delete(source.bucketsByID, chosen)
	_, err = handler.selectedBuckets(req, actor, manifest)
	require.Error(t, err)
}

// TestRuntimeBucketChoiceRemainsOptional verifies searchable fallback and the registered routing ceiling across redeployments.
func TestRuntimeBucketChoiceRemainsOptional(t *testing.T) {
	manifest, err := parseRuntimeManifest([]byte(runtimeManifestFixture))
	require.NoError(t, err)
	fixed := manifest
	fixed.Bucket = "demo"
	require.False(t, deploymentManifestWithin(fixed, manifest))
	require.False(t, deploymentManifestWithin(manifest, fixed))
	require.True(t, deploymentManifestWithin(fixed, fixed))
	replacement := fixed
	replacement.Bucket = "other"
	require.False(t, deploymentManifestWithin(replacement, fixed))
	response := httptest.NewRecorder()
	page := &runtimeConsentPage{Name: manifest.Name, Services: manifest.Services, Buckets: []store.BucketSummary{{ID: uuid.New(), Name: "demo"}}}
	renderOAuthConsentPage(response, httptest.NewRequest(http.MethodGet, "/oauth/runtime/authorize", nil), &mcpConsentHTTPStore{}, oauthprovider.AuthorizeResult{}, oauthprovider.AuthorizeRequest{}, nil, page)
	require.Contains(t, response.Body.String(), `data-searchable`)
	require.Contains(t, response.Body.String(), `<select`)
	require.Contains(t, response.Header().Get("Content-Security-Policy"), "script-src 'sha256-"+oauthConsentScriptHash+"'")
}
