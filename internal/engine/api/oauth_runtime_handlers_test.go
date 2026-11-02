package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const runtimeManifestFixture = `{"schema_version":1,"name":"support","version":"1.0.0","services":{"slack":{"version":"v1","operations":["postMessage"]}}}`

// TestRuntimeManifestRejectsWidening keeps compilation requirements distinct from arbitrary app-management configuration.
func TestRuntimeManifestRejectsWidening(t *testing.T) {
	manifest, err := parseRuntimeManifest([]byte(runtimeManifestFixture))
	require.NoError(t, err)
	doc := manifest.document("support", []string{"support"})
	require.Equal(t, "sdk", doc.Kind)
	require.NotNil(t, doc.Generate)
	require.False(t, *doc.Generate)
	cases := []string{
		strings.Replace(runtimeManifestFixture, `"schema_version":1`, `"schema_version":2`, 1),
		strings.Replace(runtimeManifestFixture, `["postMessage"]`, `["*"]`, 1),
		strings.Replace(runtimeManifestFixture, `["postMessage"]`, `["postMessage","postMessage"]`, 1),
		strings.Replace(runtimeManifestFixture, `"operations":["postMessage"]`, `"select_all":true`, 1),
		strings.Replace(runtimeManifestFixture, `"name":"support"`, `"name":"support","bucket":"*"`, 1),
		`{"schema_version":1,"name":"support","version":"1.0.0","services":{"one":{"version":"v1","operations":["list"]},"two":{"version":"v2","operations":["list"]}}}`,
	}
	// Exercise each widening attempt through the real registration parser.
	for _, raw := range cases {
		// Every malformed selection must fail before registration or consent publication.
		_, err := parseRuntimeManifest([]byte(raw))
		require.Error(t, err, raw)
	}
}

type runtimeHTTPService struct{ *registrationTestService }

// AuthorizeRuntime leaves unrelated OAuth behavior unavailable in registration fixtures.
func (*runtimeHTTPService) AuthorizeRuntime(context.Context, accesscontrol.Actor, oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error) {
	return oauthprovider.AuthorizeResult{}, oauthprovider.ErrInvalidRequest
}

// ConsentRuntime cannot publish a credential in registration-only fixtures.
func (*runtimeHTTPService) ConsentRuntime(context.Context, accesscontrol.Actor, oauthprovider.ConsentRequest, store.OAuthRuntimeGrant) (string, error) {
	return "", oauthprovider.ErrInvalidRequest
}

// TokenRuntime returns only the documented dedicated runtime projection for wire tests.
func (*runtimeHTTPService) TokenRuntime(context.Context, oauthprovider.TokenRequest) (oauthprovider.RuntimeTokenResponse, error) {
	return oauthprovider.RuntimeTokenResponse{RuntimeToken: "frt_fixture"}, nil
}

// RevokeRuntime keeps fixture revocation bounded to the specialized issuer surface.
func (*runtimeHTTPService) RevokeRuntime(context.Context, oauthprovider.RevokeRequest) error {
	return nil
}

// TestRuntimeRegistrationBindsManifestAndExcludesManagementScopes exercises the actual JSON wire contract.
func TestRuntimeRegistrationBindsManifestAndExcludesManagementScopes(t *testing.T) {
	service := &runtimeHTTPService{&registrationTestService{}}
	handler := &runtimeOAuthHandler{service: service}
	body, err := json.Marshal(map[string]string{"name": "Support", "redirect_uri": "http://127.0.0.1:1234/callback", "manifest": runtimeManifestFixture})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	handler.register(response, httptest.NewRequest(http.MethodPost, "/oauth/runtime/register", strings.NewReader(string(body))))
	require.Equal(t, http.StatusCreated, response.Code)
	require.Equal(t, []string{store.OAuthRuntimeScope}, service.request.Scopes)
	require.Contains(t, response.Body.String(), store.RuntimeManifestDigest([]byte(runtimeManifestFixture)))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.NotContains(t, response.Body.String(), "postMessage")
}

// TestRuntimeTokenWireDoesNotExposeManagementTokens prevents accidental reuse of the control token response.
func TestRuntimeTokenWireDoesNotExposeManagementTokens(t *testing.T) {
	handler := &runtimeOAuthHandler{service: &runtimeHTTPService{&registrationTestService{}}}
	request := httptest.NewRequest(http.MethodPost, "/oauth/runtime/token", strings.NewReader("grant_type=authorization_code&client_id=client&code=code"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.token(response, request)
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), `"runtime_token":"frt_fixture"`)
	require.NotContains(t, response.Body.String(), "access_token")
	require.NotContains(t, response.Body.String(), "refresh_token")
}

// TestRuntimeConsentPageShowsManifestAndExplicitBucketSelection verifies the shared template's runtime branch.
func TestRuntimeConsentPageShowsManifestAndExplicitBucketSelection(t *testing.T) {
	manifest, err := parseRuntimeManifest([]byte(runtimeManifestFixture))
	require.NoError(t, err)
	response := httptest.NewRecorder()
	bucket := uuid.New()
	source := &mcpConsentHTTPStore{}
	page := &runtimeConsentPage{Name: manifest.Name, Version: manifest.Version, Digest: store.RuntimeManifestDigest([]byte(runtimeManifestFixture)), Services: manifest.Services, Buckets: []store.BucketSummary{{ID: bucket, Name: "Support credentials"}}}
	renderOAuthConsentPage(response, httptest.NewRequest(http.MethodGet, "/oauth/runtime/authorize", nil), source, oauthprovider.AuthorizeResult{Client: store.OAuthClient{Name: "Support"}, Scope: []string{store.OAuthRuntimeScope}}, oauthprovider.AuthorizeRequest{ClientID: "client", RedirectURI: "http://127.0.0.1:1234/callback"}, nil, page)
	require.Equal(t, 200, response.Code)
	// Declining never requires selecting credentials for an application the user is refusing.
	require.Contains(t, response.Body.String(), `value="deny" formnovalidate`)
	require.Contains(t, response.Body.String(), "/oauth/runtime/authorize/consent")
	require.Contains(t, response.Body.String(), "postMessage")
	require.Contains(t, response.Body.String(), page.Digest)
	require.Contains(t, response.Body.String(), bucket.String())
	require.Contains(t, response.Body.String(), "Support credentials")
	require.NotContains(t, response.Body.String(), "runtime_token")
}

// TestRuntimeBucketSelectionsDoNotDefaultOrAlias keeps browser-submitted identities explicit.
func TestRuntimeBucketSelectionsDoNotDefaultOrAlias(t *testing.T) {
	id := uuid.NewString()
	selected, err := runtimeBucketIDs(id, []string{id})
	require.NoError(t, err)
	require.Len(t, selected, 1)
	// Reject every noncanonical form before looking up bucket metadata.
	for _, invalid := range []string{"", "default", uuid.Nil.String(), strings.ToUpper(id)} {
		// Invalid inputs cannot silently become the configured or first visible bucket.
		_, err := runtimeBucketIDs(invalid, nil)
		require.Error(t, err)
	}
}

// TestRuntimeTokenDelegationPreflight admits first publication under create while fencing existing families before apply.
func TestRuntimeTokenDelegationPreflight(t *testing.T) {
	ctx := t.Context()
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionAppAPICreate)
	source := &workspaceTestStore{}
	handler := &runtimeOAuthHandler{store: source}
	require.NoError(t, handler.preflightTokenDelegation(ctx, actor, "support"))
	familyID := uuid.New()
	_, _, err := source.CreateOrGetAppFamily(ctx, store.AppFamily{AppFamilyID: familyID, AccountID: actor.AccountID, Kind: store.AppKindSDK, CanonicalName: "support", OwnerSubjectID: actor.SubjectID, DeliveryMode: store.AppDeliveryModeAPI})
	require.NoError(t, err)
	// A stale create-only browser snapshot cannot delegate additional credentials on an existing family.
	require.Error(t, handler.preflightTokenDelegation(ctx, actor, "support"))
	// Publication rejects missing delegation before accessing a planner or committing an app.
	manifest, err := parseRuntimeManifest([]byte(runtimeManifestFixture))
	require.NoError(t, err)
	_, err = handler.publish(ctx, actor, manifest, store.RuntimeManifestDigest([]byte(runtimeManifestFixture)), []store.Bucket{{ID: uuid.New(), Name: "default"}})
	require.Error(t, err)
	require.Empty(t, source.apps)
	delegated := actorWithResourcePermissions(t, actor.WorkspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionAppAPITokensManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}})
	delegated.AccountID = actor.AccountID
	require.NoError(t, handler.preflightTokenDelegation(ctx, delegated, "support"))
	// Family delegation must not imply permission to create unrelated apps.
	require.Error(t, handler.preflightTokenDelegation(ctx, delegated, "other"))
	require.Error(t, handler.preflightTokenDelegation(ctx, actor, ""))
}
