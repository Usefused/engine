package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type mcpConsentHTTPStore struct {
	*appSelectionGraphQLTestStore
	choices []store.OAuthMCPConnectionChoice
}

// ResolveMCPRoute keeps HTTP consent attached to the same exact version as the catalogue fixture.
func (s *mcpConsentHTTPStore) ResolveMCPRoute(context.Context, uuid.UUID) (*store.MCPRouteTarget, error) {
	return &store.MCPRouteTarget{AppFamilyID: s.item.AppFamilyID, AppID: s.item.AppID, Stable: true}, nil
}

// OAuthMCPConnections returns the current account catalogue so removal is tested on consent POST too.
func (s *mcpConsentHTTPStore) OAuthMCPConnections(context.Context, uuid.UUID) ([]store.OAuthMCPConnectionChoice, error) {
	return s.choices, nil
}

// GetConnectBranding allows real consent/error templates to render without a settings database.
func (*mcpConsentHTTPStore) GetConnectBranding(context.Context) (store.ConnectBranding, error) {
	return store.DefaultConnectBranding(), nil
}

type mcpConsentHTTPService struct {
	OAuthProviderService
	grant          store.OAuthMCPGrant
	consentCalls   int
	authorizeCalls int
}

// AuthorizeMCP models registered callback validation before the HTTP layer renders or denies consent.
func (s *mcpConsentHTTPService) AuthorizeMCP(_ context.Context, _ accesscontrol.Actor, req oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error) {
	s.authorizeCalls++
	// A tampered denial callback must never become a browser redirect.
	if req.RedirectURI != "https://client.example/callback" {
		return oauthprovider.AuthorizeResult{}, oauthprovider.ErrInvalidRequest
	}
	return oauthprovider.AuthorizeResult{RequiresConsent: true, Client: store.OAuthClient{Name: "Test client"}, Scope: []string{store.OAuthMCPScope}}, nil
}

// ConsentMCP captures the actual validated grant delivered by the shared HTTP consent handler.
func (s *mcpConsentHTTPService) ConsentMCP(_ context.Context, _ accesscontrol.Actor, _ oauthprovider.ConsentRequest, grant store.OAuthMCPGrant) (string, error) {
	s.grant = grant
	s.consentCalls++
	return "https://client.example/callback?code=test", nil
}

// TokenMCP satisfies the mounted issuer contract without providing unrelated token behavior in consent tests.
func (*mcpConsentHTTPService) TokenMCP(context.Context, oauthprovider.TokenRequest, string) (oauthprovider.TokenResponse, error) {
	return oauthprovider.TokenResponse{}, oauthprovider.ErrInvalidGrant
}

// RegisterMCPClient preserves standard registration response identities for mounted-handler tests.
func (*mcpConsentHTTPService) RegisterMCPClient(_ context.Context, req oauthprovider.MCPRegistration) (oauthprovider.CreateClientResult, error) {
	return oauthprovider.CreateClientResult{Client: store.OAuthClient{ClientID: "foc_public", Name: req.ClientName, RedirectURIs: req.RedirectURIs}}, nil
}

type mcpConsentHTTPFixture struct {
	router  chi.Router
	store   *mcpConsentHTTPStore
	service *mcpConsentHTTPService
	session *browserSessionHandlerFixture
	form    url.Values
}

// newMCPConsentHTTPFixture reuses the exact-operation and browser-session fixtures across real mounted routes.
func newMCPConsentHTTPFixture(t *testing.T) mcpConsentHTTPFixture {
	t.Helper()
	source := &mcpConsentHTTPStore{appSelectionGraphQLTestStore: newMCPAppOperationsGraphQLFixture()}
	first := source.item.Selections[0].ServiceID
	second := source.item.Selections[1].ServiceID
	source.services = []store.AppServiceSummary{{ServiceID: first, ServiceName: "Customers"}, {ServiceID: second, ServiceName: "Tickets"}}
	source.item.Selections[0].AuthType, source.item.Selections[0].AuthName = "oauth", "customers"
	source.choices = []store.OAuthMCPConnectionChoice{{ServiceID: first, AuthName: "customers", ConnectionID: uuid.New(), Value: "account-a", Label: "Support account"}}
	session := browserSessionHandlerTestFixture(t)
	session.actor = actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionAppMCPUse, accesscontrol.PermissionAppMCPTokensManage)
	session.actor.AccountID = source.accountID
	service := &mcpConsentHTTPService{}
	router := chi.NewRouter()
	MountMCPOAuthRoutes(router, service, source, session, session.cookies, "https://example.com")
	form := url.Values{"client_id": {"client"}, "redirect_uri": {"https://client.example/callback"}, "resource": {"https://example.com/mcp/" + source.item.AppFamilyID.String()}, "app_id": {source.item.AppID.String()}, "state": {"state"}, "code_challenge": {"challenge"}, "code_challenge_method": {"S256"}, "decision": {"allow"}, "operation": {"customers.list"}, "account_0": {"account-a"}}
	return mcpConsentHTTPFixture{router, source, service, session, form}
}

// request submits real cookies and form data so the common HTTP guards run before MCP policy.
func (f mcpConsentHTTPFixture) request(method, path, origin string, cookie bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://example.com"+path, strings.NewReader(f.form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", origin)
	// Cookie absence exercises the actual browser-authentication guard, not a simulated policy denial.
	if cookie {
		issued := httptest.NewRecorder()
		f.session.cookies.SetSession(issued, f.session.credential.RawKey, f.session.credential.ExpiresAt)
		request.AddCookie(issued.Result().Cookies()[0])
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

// TestOAuthSharedConsentPreservesMCPGrant proves the mounted shared handler retains exact service/actions and account binding.
func TestOAuthSharedConsentPreservesMCPGrant(t *testing.T) {
	f := newMCPConsentHTTPFixture(t)
	response := f.request(http.MethodGet, "/mcp/oauth/authorize?"+f.form.Encode(), "", true)
	require.Equal(t, http.StatusOK, response.Code)
	// no-referrer makes native browser form POSTs send Origin: null and fail the shared CSRF guard.
	require.Equal(t, "same-origin", response.Header().Get("Referrer-Policy"))
	require.Contains(t, response.Body.String(), "customers.list")
	require.Contains(t, response.Body.String(), "tickets.create")
	require.NotRegexp(t, `<input\b[^>]*\schecked(?:\s|=|>)`, response.Body.String())
	// Hash the rendered bytes so template processing cannot silently disable the permission picker under CSP.
	_, script, found := strings.Cut(response.Body.String(), "<script>")
	require.True(t, found)
	script, _, found = strings.Cut(script, "</script>")
	require.True(t, found)
	digest := sha256.Sum256([]byte(script))
	require.Contains(t, response.Header().Get("Content-Security-Policy"), "script-src 'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'")
	response = f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
	require.Equal(t, http.StatusOK, response.Code)
	// A no-store handoff ends form submission before navigating to the validated client callback.
	require.Contains(t, response.Body.String(), `http-equiv="refresh"`)
	require.Contains(t, response.Body.String(), `https://client.example/callback?code=test`)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Contains(t, response.Header().Get("Content-Security-Policy"), "form-action 'self'")
	require.Equal(t, 1, f.service.consentCalls)
	require.Equal(t, []string{"customers.list"}, f.service.grant.Operations)
	require.Equal(t, f.store.item.AppFamilyID, f.service.grant.AppFamilyID)
	require.Equal(t, f.store.item.AppID, f.service.grant.AppID)
	require.Equal(t, f.form.Get("resource"), f.service.grant.Resource)
	require.Equal(t, []store.OAuthMCPConnectionBinding{{ServiceID: f.store.choices[0].ServiceID, AuthName: "customers", ConnectionID: f.store.choices[0].ConnectionID}}, f.service.grant.Connections)
}

// TestOAuthSharedConsentRejectsMCPPolicyExpansion exercises policy tampering through the mounted HTTP adapter.
func TestOAuthSharedConsentRejectsMCPPolicyExpansion(t *testing.T) {
	cases := []struct{ name, key, value string }{
		{"unknown action", "operation", "delete.everything"},
		{"empty selection", "operation", ""},
		{"foreign account", "account_0", "account-b"},
		{"stale version", "app_id", uuid.NewString()},
		{"foreign resource", "resource", "https://attacker.example/mcp/" + uuid.NewString()},
		{"unknown decision", "decision", "maybe"},
	}
	// Every mutation begins from a valid consent form, isolating the boundary under test.
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newMCPConsentHTTPFixture(t)
			f.form.Set(test.key, test.value)
			response := f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Zero(t, f.service.consentCalls)
		})
	}
}

// TestOAuthSharedConsentBrowserGuards rejects anonymous and cross-origin POSTs before protocol or catalogue access.
func TestOAuthSharedConsentBrowserGuards(t *testing.T) {
	cases := []struct {
		name, origin string
		cookie       bool
		status       int
	}{
		{"anonymous", "https://example.com", false, http.StatusUnauthorized},
		{"cross origin", "https://attacker.example", true, http.StatusForbidden},
		{"opaque origin", "null", true, http.StatusForbidden},
		{"missing origin", "", true, http.StatusForbidden},
	}
	// Every browser-boundary failure must leave the MCP policy adapter untouched.
	for _, test := range cases {
		// Exercise each guard independently with otherwise valid consent and catalogue state.
		t.Run(test.name, func(t *testing.T) {
			f := newMCPConsentHTTPFixture(t)
			response := f.request(http.MethodPost, "/mcp/oauth/consent", test.origin, test.cookie)
			require.Equal(t, test.status, response.Code)
			require.Zero(t, f.service.authorizeCalls)
			require.Zero(t, f.service.consentCalls)
		})
	}
}

// TestOAuthSharedConsentCancellationValidatesCallback preserves safe denial without issuing a grant.
func TestOAuthSharedConsentCancellationValidatesCallback(t *testing.T) {
	f := newMCPConsentHTTPFixture(t)
	f.form.Set("decision", "deny")
	response := f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
	require.Equal(t, http.StatusOK, response.Code)
	// Cancellation uses the same safe browser handoff without minting any grant.
	require.Contains(t, response.Body.String(), "https://client.example/callback?error=access_denied&amp;state=state")
	require.Zero(t, f.service.consentCalls)
	f.form.Set("redirect_uri", "https://attacker.example/callback")
	response = f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Empty(t, response.Header().Get("Location"))
	require.Zero(t, f.service.consentCalls)
}

// TestOAuthSharedRegistrationPreservesRemoteDialect keeps RFC 7591 extensions and public-client responses distinct from strict CLI input.
func TestOAuthSharedRegistrationPreservesRemoteDialect(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"public with extension", `{"client_name":"Remote","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none","client_uri":"https://client.example"}`, http.StatusCreated},
		{"trailing JSON", `{} {}`, http.StatusBadRequest},
		{"malformed", `{`, http.StatusBadRequest},
		{"oversized", `{"client_name":"` + strings.Repeat("x", 17000) + `"}`, http.StatusBadRequest},
	}
	// The shared transport must preserve each dialect's parsing and credential disclosure contract.
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newMCPConsentHTTPFixture(t)
			response := httptest.NewRecorder()
			f.router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "https://example.com/mcp/oauth/register", strings.NewReader(test.body)))
			require.Equal(t, test.status, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			require.NotContains(t, response.Body.String(), "client_secret")
		})
	}
}

// TestOAuthSharedConsentDoesNotGrantUnselectedAccounts keeps unrelated service account choices out of the minted grant.
func TestOAuthSharedConsentDoesNotGrantUnselectedAccounts(t *testing.T) {
	f := newMCPConsentHTTPFixture(t)
	f.form.Set("operation", "tickets.create")
	response := f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []string{"tickets.create"}, f.service.grant.Operations)
	require.Empty(t, f.service.grant.Connections)
}

// TestOAuthSharedConsentRechecksAccountAndPermissions denies changes made after a consent page was displayed.
func TestOAuthSharedConsentRechecksAccountAndPermissions(t *testing.T) {
	f := newMCPConsentHTTPFixture(t)
	f.store.choices = nil
	response := f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Zero(t, f.service.consentCalls)
	f = newMCPConsentHTTPFixture(t)
	f.session.actor = actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionAppMCPUse)
	f.session.actor.AccountID = f.store.accountID
	response = f.request(http.MethodPost, "/mcp/oauth/consent", "https://example.com", true)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Zero(t, f.service.consentCalls)
}

// TestOAuthSharedManagementConsentPresentation keeps management permissions and form submission separate from MCP actions.
func TestOAuthSharedManagementConsentPresentation(t *testing.T) {
	f := newMCPConsentHTTPFixture(t)
	response := httptest.NewRecorder()
	renderOAuthConsentPage(response, httptest.NewRequest(http.MethodGet, "https://example.com/oauth/authorize", nil), f.store,
		oauthprovider.AuthorizeResult{Client: store.OAuthClient{Name: "Management client"}, Scope: []string{"service.read"}},
		oauthprovider.AuthorizeRequest{ClientID: "client", RedirectURI: "https://client.example/callback", State: "state"}, nil)
	require.Equal(t, http.StatusOK, response.Code)
	// Management consent uses the same native form submission and requires the same Origin preservation.
	require.Equal(t, "same-origin", response.Header().Get("Referrer-Policy"))
	require.Contains(t, response.Body.String(), `action="/oauth/authorize/consent"`)
	require.Contains(t, response.Body.String(), "View connected services")
	require.NotContains(t, response.Body.String(), `name="operation"`)
	require.NotContains(t, response.Body.String(), `name="resource"`)
	require.Contains(t, response.Header().Get("Content-Security-Policy"), "form-action 'self'")
}
