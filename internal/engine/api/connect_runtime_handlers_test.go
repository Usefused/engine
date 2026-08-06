package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestStartConnectSessionHandlerCreatesAuthorizationURL verifies the start
// endpoint emits browser-safe auth material while storing server-only state.
func TestStartConnectSessionHandlerCreatesAuthorizationURL(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	router := buildConnectRuntimeRouter(fixture)
	appID := attachConnectTestArtifact(&fixture)

	body := bytes.NewReader([]byte(`{"end_user_ref":"user_123","created_by_app_id":"` + appID.String() + `","return_url":"https://app.example.com/oauth/done"}`))
	req := httptest.NewRequest(http.MethodPost, fixture.startPath(), body)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(fixture.store.createdSessions) != 1 {
		t.Fatalf("expected one connect session, got %#v", fixture.store.createdSessions)
	}
	values := authorizeURLValues(t, rr.Body.Bytes())
	if values.Get("client_id") != "client-id" || values.Get("response_type") != "code" {
		t.Fatalf("unexpected authorize URL query: %#v", values)
	}
	if values.Get("code_challenge") == "" || values.Get("state") == "" || values.Get("nonce") == "" {
		t.Fatalf("expected state, nonce, and PKCE challenge in authorize URL: %#v", values)
	}
	if fixture.store.createdSessions[0].EndUserRef != "user_123" {
		t.Fatalf("expected end_user_ref to be stored, got %#v", fixture.store.createdSessions[0])
	}
	if fixture.store.createdSessions[0].ReturnURL != "https://app.example.com/oauth/done" {
		t.Fatalf("expected return_url to be stored, got %#v", fixture.store.createdSessions[0])
	}
}

// attachConnectTestArtifact gives connect entrypoint tests a real immutable
// scope because production rejects attribution to an unknown SDK/MCP ID.
func attachConnectTestArtifact(fixture *connectRuntimeFixture) uuid.UUID {
	appID := uuid.New()
	selections, _ := json.Marshal([]models.SDKSelection{{ServiceID: fixture.serviceID}})
	fixture.store.appRuntimes = map[uuid.UUID]*store.AppRuntime{
		appID: {AccountID: fixture.store.accountID, AppID: appID, BucketID: fixture.bucketID, Selections: selections},
	}
	return appID
}

// TestResolveConnectScopesNarrowsAndNormalizes proves callers can request a
// least-privilege subset without duplicate or order-dependent consent values.
func TestResolveConnectScopesNarrowsAndNormalizes(t *testing.T) {
	auth := fusedobject.AuthConfig{Type: "oauth2", Scopes: []string{"write", "read", "offline_access"}}
	scopes, err := resolveConnectScopes(auth, []string{"write", "read", "write"})
	if err != nil {
		t.Fatalf("resolve scopes: %v", err)
	}
	if strings.Join(scopes, " ") != "read write" {
		t.Fatalf("unexpected normalized scopes: %#v", scopes)
	}
}

// TestResolveConnectScopesRejectsUndeclared prevents an app caller from using
// startConnectSession to expand beyond the pinned service contract.
func TestResolveConnectScopesRejectsUndeclared(t *testing.T) {
	auth := fusedobject.AuthConfig{Type: "oauth2", Scopes: []string{"read"}}
	if _, err := resolveConnectScopes(auth, []string{"admin"}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("expected undeclared scope rejection, got %v", err)
	}
}

// TestResolveConnectScopesRequiresOpenID keeps OIDC nonce/id-token validation
// active even when a caller narrows the provider's broader scope catalogue.
func TestResolveConnectScopesRequiresOpenID(t *testing.T) {
	auth := fusedobject.AuthConfig{Type: "openIdConnect", Scopes: []string{"openid", "profile"}}
	if _, err := resolveConnectScopes(auth, []string{"profile"}); err == nil || !strings.Contains(err.Error(), "include openid") {
		t.Fatalf("expected openid requirement, got %v", err)
	}
}

func TestArtifactConnectScopePolicyIsAppliedBeforeProviderScopes(t *testing.T) {
	fixture := newConnectAdminFixture()
	appID := uuid.New()
	selections, _ := json.Marshal([]models.SDKSelection{{ServiceID: fixture.serviceID, ConnectScopes: []string{"read"}}})
	fixture.store.appRuntimes = map[uuid.UUID]*store.AppRuntime{
		appID: {AccountID: fixture.store.accountID, AppID: appID, BucketID: fixture.bucketID, Selections: selections},
	}

	scopes, err := applyAppConnectScopePolicy(context.Background(), fixture.store, fixture.bucketID, fixture.serviceID, appID, nil)
	if err != nil || strings.Join(scopes, " ") != "read" {
		t.Fatalf("artifact policy = %#v, err = %v", scopes, err)
	}
	if _, err := applyAppConnectScopePolicy(context.Background(), fixture.store, fixture.bucketID, fixture.serviceID, appID, []string{"write"}); err == nil {
		t.Fatal("expected a scope outside the artifact policy to be rejected")
	}
}

// TestStartConnectSessionHandlerRejectsUndeclaredScope verifies the HTTP
// boundary preserves policy errors as caller-visible 400 responses.
func TestStartConnectSessionHandlerRejectsUndeclaredScope(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	router := buildConnectRuntimeRouter(fixture)
	body := bytes.NewReader([]byte(`{"end_user_ref":"user_123","scopes":["admin"]}`))
	req := httptest.NewRequest(http.MethodPost, fixture.startPath(), body)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "not declared") {
		t.Fatalf("expected undeclared scope 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(fixture.store.createdSessions) != 0 {
		t.Fatalf("scope validation must happen before persistence: %#v", fixture.store.createdSessions)
	}
}

// TestConnectCallbackHandlerStoresEncryptedAuthConnection proves callback
// exchange consumes the session and stores provider tokens encrypted.
func TestConnectCallbackHandlerStoresEncryptedAuthConnection(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	state := "state-value"
	verifier := "pkce-verifier"
	fixture.store.session = connectRuntimeSession(t, fixture, state, verifier)
	fixture.store.session.RequestedScopes = []string{"openid", "profile"}
	fixture.store.session.ReturnURL = "https://app.example.com/oauth/done?existing=1"
	restoreClient := replaceDefaultHTTPClient(tokenRoundTripper(t, verifier))
	defer restoreClient()

	router := buildConnectRuntimeRouter(fixture)
	req := httptest.NewRequest(http.MethodGet, "/workspace/connect/callback?state="+url.QueryEscape(state)+"&code=auth-code", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rr.Code, rr.Body.String())
	}
	if fixture.store.markedStateHash != connectHash(state) {
		t.Fatalf("expected session to be consumed, got %q", fixture.store.markedStateHash)
	}
	assertRuntimeAuthConnectionEncrypted(t, fixture.store.savedConnection, fixture)
	assertCallbackRedirectLocation(t, rr.Header().Get("Location"), fixture.store.savedConnection.ID)
}

// TestConnectCallbackHandlerDiscoversResources covers token exchange followed
// by provider discovery and one atomic store reconciliation.
func TestConnectCallbackHandlerDiscoversResources(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh-token","token_type":"Bearer","expires_in":3600}`))
		case "/resources":
			if r.Header.Get("Authorization") != "Bearer fresh-token" {
				t.Fatalf("discovery token header = %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"cloud-a","name":"Acme"},{"id":"cloud-b","name":"Beta"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	fixture := newConnectRuntimeFixture(t)
	state := "discovery-state"
	fixture.store.session = connectRuntimeSession(t, fixture, state, "pkce-verifier")
	fixture.store.session.RequestedScopes = []string{"account:read"}
	fixture.verifier.serviceMetadata.ServiceVersionID = uuid.New()
	fixture.verifier.serviceMetadata.BaseURL = provider.URL
	fixture.verifier.serviceMetadata.AuthConfigs[0].Type = "oauth2"
	fixture.store.savedConfig.AuthType = "oauth"
	// This fixture exercises OAuth resource discovery; retaining the shared
	// fixture's openid scope would correctly require an OIDC ID token and nonce.
	fixture.verifier.serviceMetadata.AuthConfigs[0].Scopes = nil
	fixture.verifier.serviceMetadata.AuthConfigs[0].TokenURL = provider.URL + "/token"
	fixture.verifier.serviceMetadata.ConnectConfig = &fusedobject.ServiceConnectConfig{
		ResourceDiscovery: &fusedobject.ResourceDiscoveryConfig{
			OperationID: "getAccessibleResources", IDPath: "$[*].id", NamePath: "$[*].name",
			BaseURLTemplate: "https://api.atlassian.com/ex/jira/{id}", ResourceType: "jira_site",
			AutoRun: "after_oauth_callback", AllowedHosts: []string{"api.atlassian.com"},
		},
	}
	fixture.verifier.discoveryEndpoint = &fusedobject.Endpoint{Name: "getAccessibleResources", Method: http.MethodGet, Path: "/resources"}

	router := buildConnectRuntimeRouter(fixture)
	req := httptest.NewRequest(http.MethodGet, "/workspace/connect/callback?state="+url.QueryEscape(state)+"&code=auth-code", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	// Without a customer return URL the Engine owns the final browser response,
	// so successful discovery should render its completion page directly.
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(fixture.store.reconciledResources) != 2 || fixture.store.reconciledResources[1].ProviderResourceID != "cloud-b" {
		t.Fatalf("unexpected reconciled resources: %#v", fixture.store.reconciledResources)
	}
	if fixture.store.savedConnection.ScopeSource != "request" || strings.Join(fixture.store.savedConnection.Scopes, " ") != "account:read" {
		t.Fatalf("expected requested scope fallback, got source=%q scopes=%#v", fixture.store.savedConnection.ScopeSource, fixture.store.savedConnection.Scopes)
	}
}

// TestSelectRuntimeOAuthConfigMatchesConfiguredFamily protects mixed-scheme
// services from silently choosing whichever declaration happened to come first.
func TestSelectRuntimeOAuthConfigMatchesConfiguredFamily(t *testing.T) {
	auths := fusedobject.AuthConfigs{
		{Name: "oauthScheme", Type: "oauth2", AuthorizationURL: "https://oauth.example/authorize", TokenURL: "https://oauth.example/token"},
		{Name: "oidcScheme", Type: "openIdConnect", AuthorizationURL: "https://oidc.example/authorize", TokenURL: "https://oidc.example/token"},
	}
	auth, err := selectRuntimeOAuthConfig(auths, "oidc")
	if err != nil {
		t.Fatalf("select oidc auth: %v", err)
	}
	if auth.Name != "oidcScheme" {
		t.Fatalf("selected %q, want oidcScheme", auth.Name)
	}
}

// TestRediscoverConnectionResourcesReusesConnectedToken covers the manual
// lifecycle path without exposing the provider token through GraphQL.
func TestRediscoverConnectionResourcesReusesConnectedToken(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/resources" || r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("unexpected discovery request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`[{"id":"portal-1","name":"Acme"}]`))
	}))
	defer provider.Close()
	fixture := newConnectRuntimeFixture(t)
	wrapped, dek, err := store.WrapDEK(fixture.masterKey)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := store.EncryptWithDEK(dek, "access-token")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	connection := &store.AuthConnection{
		ID: uuid.New(), BucketID: fixture.bucketID, ServiceID: fixture.serviceID,
		EndUserRef: "user-1", AuthType: "oauth", EncryptedDEK: wrapped,
		EncryptedAccessToken: encrypted, TokenType: "Bearer", ExpiresAt: &expires, RefreshState: "ok",
	}
	fixture.store.savedConnection = connection
	fixture.verifier.serviceMetadata.ServiceVersionID = uuid.New()
	fixture.verifier.serviceMetadata.BaseURL = provider.URL
	fixture.verifier.serviceMetadata.AuthConfigs = fusedobject.AuthConfigs{{
		Name: "oauthScheme", Type: "oauth2", AuthorizationURL: provider.URL + "/authorize", TokenURL: provider.URL + "/token",
	}}
	fixture.verifier.serviceMetadata.ConnectConfig = &fusedobject.ServiceConnectConfig{
		AuthType: "oauth",
		ResourceDiscovery: &fusedobject.ResourceDiscoveryConfig{
			OperationID: "listPortals", IDPath: "$[*].id", NamePath: "$[*].name", ResourceType: "portal",
		},
	}
	fixture.verifier.discoveryEndpoint = &fusedobject.Endpoint{Name: "listPortals", Method: http.MethodGet, Path: "/resources"}
	resources, err := rediscoverConnectionResources(context.Background(), fixture.store, fixture.verifier, fixture.masterKey, connection)
	if err != nil {
		t.Fatalf("rediscover resources: %v", err)
	}
	if len(resources) != 1 || resources[0].ProviderResourceID != "portal-1" || resources[0].BaseURL != "" {
		t.Fatalf("resources = %#v", resources)
	}
}

type connectRuntimeFixture struct {
	store     *connectAdminMockStore
	verifier  *mockVerifier
	masterKey []byte
	bucketID  uuid.UUID
	serviceID uuid.UUID
}

// newConnectRuntimeFixture shares the same store/verifier setup across start
// and callback tests so they exercise one coherent bucket/service boundary.
func newConnectRuntimeFixture(t *testing.T) connectRuntimeFixture {
	t.Helper()
	admin := newConnectAdminFixture()
	cfg := encryptedRuntimeConnectConfig(t, admin)
	admin.store.savedConfig = &cfg
	metadata := &fusedobject.ServiceMetadata{
		ID: admin.serviceID,
		AuthConfigs: fusedobject.AuthConfigs{{
			Name:             "bearerAuth",
			Type:             "openIdConnect",
			TokenURL:         "https://provider.example/token",
			AuthorizationURL: "https://provider.example/authorize",
			Scopes:           []string{"openid", "profile"},
		}},
	}
	return connectRuntimeFixture{
		store:     admin.store,
		verifier:  &mockVerifier{serviceMetadata: metadata},
		masterKey: admin.masterKey,
		bucketID:  admin.bucketID,
		serviceID: admin.serviceID,
	}
}

// buildConnectRuntimeRouter mounts the real workspace routes so tests cover
// chi params and auth middleware behavior, not just handler internals.
func buildConnectRuntimeRouter(f connectRuntimeFixture) http.Handler {
	r := newControlTestRouter(f.store.accountID)
	r.Mount("/workspace", WorkspaceHandler(f.store, f.verifier, f.masterKey))
	return r
}

// startPath builds the bucket/service scoped route from fixture IDs to avoid
// tests drifting from router path shape.
func (f connectRuntimeFixture) startPath() string {
	return "/workspace/buckets/" + f.bucketID.String() + "/services/" + f.serviceID.String() + "/connect/sessions"
}

// encryptedRuntimeConnectConfig uses production encryption helpers so tests
// catch DEK/ciphertext shape changes in connect config storage.
func encryptedRuntimeConnectConfig(t *testing.T, fixture connectAdminFixture) store.ConnectConfig {
	t.Helper()
	cfg, err := encryptConnectConfig(fixture.bucketID, fixture.serviceID, resolvedConnectConfigFields{
		AuthType:     "oidc",
		Enabled:      true,
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURI:  "https://engine.example.com/workspace/connect/callback",
	}, fixture.masterKey)
	if err != nil {
		t.Fatalf("encrypt connect config: %v", err)
	}
	return cfg
}

// authorizeURLValues extracts the provider query because the security contract
// lives in redirect parameters, not only in HTTP status.
func authorizeURLValues(t *testing.T, body []byte) url.Values {
	t.Helper()
	var resp connectSessionStartResponse
	if err := jsonDecode(body, &resp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	parsed, err := url.Parse(resp.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	return parsed.Query()
}

// connectRuntimeSession builds a callback-ready session with encrypted PKCE so
// callback tests exercise the same decrypt path as live browser flows.
func connectRuntimeSession(t *testing.T, fixture connectRuntimeFixture, state, verifier string) *store.ConnectSession {
	t.Helper()
	encrypted, err := encryptConnectPKCEVerifier(fixture.masterKey, verifier)
	if err != nil {
		t.Fatalf("encrypt PKCE verifier: %v", err)
	}
	return &store.ConnectSession{
		ID:                    uuid.New(),
		BucketID:              fixture.bucketID,
		ServiceID:             fixture.serviceID,
		EndUserRef:            "user_123",
		StateHash:             connectHash(state),
		NonceHash:             connectHash("nonce-value"),
		EncryptedDEK:          encrypted.wrappedDEK,
		EncryptedPKCEVerifier: encrypted.value,
		ExpiresAt:             time.Now().UTC().Add(time.Minute),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip lets tests replace the provider HTTP call without running a second
// server, keeping token exchange assertions local and deterministic.
func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// tokenRoundTripper validates the token request before returning fixture token
// material, so callback storage only succeeds for correct PKCE/code exchange.
func tokenRoundTripper(t *testing.T, wantVerifier string) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(raw))
		if values.Get("code_verifier") != wantVerifier || values.Get("code") != "auth-code" {
			t.Fatalf("unexpected token request form: %#v", values)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"access-token","refresh_token":"refresh-token","id_token":"header.` + testJWTClaims(t) + `.sig","token_type":"Bearer","expires_in":3600,"refresh_token_expires_in":7200,"scope":"openid profile"}`)),
		}, nil
	})
}

// replaceDefaultHTTPClient scopes the global client swap so callback tests do
// not leak fake provider behavior into unrelated cases.
func replaceDefaultHTTPClient(transport http.RoundTripper) func() {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	return func() { http.DefaultClient = previous }
}

// testJWTClaims emits a nonce-bearing payload so OIDC callback validation and
// claim persistence are both covered.
func testJWTClaims(t *testing.T) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://issuer.example","sub":"subject_123","nonce":"nonce-value"}`))
}

// assertRuntimeAuthConnectionEncrypted checks both identity metadata and
// ciphertext contents so tests fail on token leakage, not just bad IDs.
func assertRuntimeAuthConnectionEncrypted(t *testing.T, conn *store.AuthConnection, fixture connectRuntimeFixture) {
	t.Helper()
	if conn == nil {
		t.Fatal("expected auth connection to be saved")
	}
	if conn.BucketID != fixture.bucketID || conn.ServiceID != fixture.serviceID || conn.EndUserRef != "user_123" {
		t.Fatalf("unexpected auth connection identity: %#v", conn)
	}
	if conn.EncryptedAccessToken == "access-token" || conn.EncryptedRefreshToken == "refresh-token" {
		t.Fatalf("auth connection tokens must be encrypted: %#v", conn)
	}
	dek, err := store.UnwrapDEK(fixture.masterKey, conn.EncryptedDEK)
	if err != nil {
		t.Fatalf("unwrap auth connection DEK: %v", err)
	}
	assertDecryptedValue(t, dek, conn.EncryptedAccessToken, "access-token")
	assertDecryptedValue(t, dek, conn.EncryptedRefreshToken, "refresh-token")
	if conn.Issuer != "https://issuer.example" || conn.Subject != "subject_123" {
		t.Fatalf("expected OIDC claims to be stored, got %#v", conn)
	}
	if conn.ScopeSource != "provider" || strings.Join(conn.Scopes, " ") != "openid profile" {
		t.Fatalf("expected provider scopes to be stored, got source=%q scopes=%#v", conn.ScopeSource, conn.Scopes)
	}
	if conn.RefreshTokenExpiresAt == nil {
		t.Fatal("expected refresh token expiry metadata to be stored")
	}
}

// assertCallbackRedirectLocation verifies browser callbacks return only a
// connection handle, leaving metadata reads to the server-side getConnection API.
func assertCallbackRedirectLocation(t *testing.T, raw string, connectionID uuid.UUID) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse callback redirect: %v", err)
	}
	query := parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != "app.example.com" || query.Get("existing") != "1" {
		t.Fatalf("unexpected callback redirect target: %s", raw)
	}
	if query.Get("fused_connect") != "success" || query.Get("connection_id") != connectionID.String() {
		t.Fatalf("unexpected callback redirect query: %s", raw)
	}
}

// assertDecryptedValue proves stored ciphertext can be recovered by Engine
// while still keeping raw provider material out of persisted fields.
func assertDecryptedValue(t *testing.T, dek []byte, encrypted, want string) {
	t.Helper()
	got, err := store.DecryptWithDEK(dek, encrypted)
	if err != nil || got != want {
		t.Fatalf("decrypt token = %q want %q err=%v", got, want, err)
	}
}

// jsonDecode keeps response decoding terse so each test can focus on auth
// invariants rather than fixture plumbing.
func jsonDecode(body []byte, dest any) error {
	return json.NewDecoder(bytes.NewReader(body)).Decode(dest)
}
