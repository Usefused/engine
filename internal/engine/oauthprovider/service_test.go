package oauthprovider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// fakeOAuthStore is an in-memory double for store.OAuthClientStore used to
// exercise the service's own request validation, actor guards, and
// error-translation logic without a database. The security-critical
// transactional invariants it fakes out here (single-use codes, PKCE
// verification, refresh-token reuse cascade) are instead covered against a
// real Postgres instance in postgres_store_oauth_provider_test.go, since
// those live in the store layer, not this package.
type fakeOAuthStore struct {
	client           store.OAuthClient
	clientSecret     string
	clientErr        error
	consent          store.OAuthConsent
	consentFound     bool
	consentErr       error
	recordedConsent  store.OAuthConsentGrant
	recordedCode     store.OAuthAuthorizationCodeIssue
	recordConsentErr error
	exchangeResult   store.OAuthTokenMetadata
	exchangeErr      error
	recordedExchange store.OAuthCodeExchange
	rotateResult     store.OAuthTokenMetadata
	rotateErr        error
	recordedRotate   store.OAuthRefreshExchange
	revokeFound      bool
	revokeErr        error
	createResult     store.OAuthClientMutationResult
	createErr        error
	recordedCreate   store.OAuthClientRegistration
	listClients      []store.OAuthClient
	revokeClientRev  int64
	revokeClientErr  error
	connectedApps    []store.OAuthConnectedApp
	connectedAppsErr error
	revokeAppRev     int64
	revokeAppErr     error
}

func (f *fakeOAuthStore) CreateOAuthClient(_ context.Context, input store.OAuthClientRegistration) (store.OAuthClientMutationResult, error) {
	f.recordedCreate = input
	return f.createResult, f.createErr
}

func (f *fakeOAuthStore) ListOAuthClients(_ context.Context) ([]store.OAuthClient, error) {
	return f.listClients, nil
}

func (f *fakeOAuthStore) RevokeOAuthClient(_ context.Context, _ uuid.UUID, _ store.MutationActor) (int64, error) {
	return f.revokeClientRev, f.revokeClientErr
}

func (f *fakeOAuthStore) GetOAuthClientByPublicID(_ context.Context, _ string) (store.OAuthClient, string, error) {
	return f.client, f.clientSecret, f.clientErr
}

func (f *fakeOAuthStore) GetOAuthUserConsent(_ context.Context, _, _ uuid.UUID) (store.OAuthConsent, bool, error) {
	return f.consent, f.consentFound, f.consentErr
}

func (f *fakeOAuthStore) RecordOAuthConsentAndIssueCode(_ context.Context, consent store.OAuthConsentGrant, code store.OAuthAuthorizationCodeIssue) error {
	f.recordedConsent, f.recordedCode = consent, code
	return f.recordConsentErr
}

func (f *fakeOAuthStore) ExchangeOAuthAuthorizationCode(_ context.Context, input store.OAuthCodeExchange, _ time.Time) (store.OAuthTokenMetadata, error) {
	f.recordedExchange = input
	return f.exchangeResult, f.exchangeErr
}

func (f *fakeOAuthStore) RotateOAuthRefreshToken(_ context.Context, input store.OAuthRefreshExchange, _ time.Time) (store.OAuthTokenMetadata, error) {
	f.recordedRotate = input
	return f.rotateResult, f.rotateErr
}

func (f *fakeOAuthStore) RevokeOAuthTokenByHash(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	return f.revokeFound, f.revokeErr
}

func (f *fakeOAuthStore) ListOAuthConnectedApps(_ context.Context, _ uuid.UUID) ([]store.OAuthConnectedApp, error) {
	return f.connectedApps, f.connectedAppsErr
}

func (f *fakeOAuthStore) RevokeOAuthConnectedApp(_ context.Context, _, _ uuid.UUID, _ store.MutationActor) (int64, error) {
	return f.revokeAppRev, f.revokeAppErr
}

func (f *fakeOAuthStore) ExpireOAuthArtifacts(_ context.Context, _ time.Time, _ int) (int, error) {
	return 0, nil
}

var _ store.OAuthClientStore = (*fakeOAuthStore)(nil)

type fakeRevisionSink struct{ revision int64 }

func (f *fakeRevisionSink) SetRevision(revision int64) bool { f.revision = revision; return true }

// browserActor returns an authenticated end-user actor whose Authorization
// snapshot grants exactly the given permissions workspace-wide, matching the
// shape produced by a real managed login (browserauth.IsBrowserSessionActor
// recognizes CredentialSource "managed_login").
func browserActor(t *testing.T, permissions ...accesscontrol.Permission) accesscontrol.Actor {
	t.Helper()
	grants := make([]accesscontrol.Grant, 0, len(permissions))
	for _, permission := range permissions {
		grants = append(grants, accesscontrol.Grant{
			Permission: permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: uuid.New()},
		})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot: %v", err)
	}
	return accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), CredentialSource: "managed_login",
		AuthenticationMethod: "google", Kind: accesscontrol.SubjectUser, Authorization: snapshot,
	}
}

const validCodeChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

func confidentialClient() store.OAuthClient {
	return store.OAuthClient{
		ID: uuid.New(), Name: "Test App", ClientID: "foc_test", ClientType: store.OAuthClientConfidential,
		RedirectURIs: []string{"https://app.example.com/callback"}, AllowedScopes: []string{"service.read", "service.consume"},
	}
}

func authorizeRequest(client store.OAuthClient) AuthorizeRequest {
	return AuthorizeRequest{
		ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0], ResponseType: "code",
		Scope: []string{"service.read"}, State: "xyz", CodeChallenge: validCodeChallenge, CodeChallengeMethod: "S256",
	}
}

func newTestService(t *testing.T, repository *fakeOAuthStore) (*Service, *fakeRevisionSink) {
	t.Helper()
	sink := &fakeRevisionSink{}
	service, err := NewService(repository, sink)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service, sink
}

func TestCreateClientConfidentialIssuesHashedSecretOnly(t *testing.T) {
	repository := &fakeOAuthStore{createResult: store.OAuthClientMutationResult{
		Client: store.OAuthClient{ID: uuid.New(), ClientType: store.OAuthClientConfidential}, AuthorizationRevision: 4,
	}}
	service, sink := newTestService(t, repository)
	result, err := service.CreateClient(t.Context(), browserActor(t), CreateClientInput{
		Name: "Third-party app", ClientType: store.OAuthClientConfidential,
		RedirectURIs: []string{"https://app.example.com/callback"}, AllowedScopes: []string{"service.read"},
	})
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if result.ClientSecret == "" {
		t.Fatal("confidential client did not receive a raw secret")
	}
	if repository.recordedCreate.ClientSecretHash == "" || repository.recordedCreate.ClientSecretHash == result.ClientSecret {
		t.Fatal("store did not receive a hashed (not raw) secret")
	}
	if got := hashSecret(result.ClientSecret); got != repository.recordedCreate.ClientSecretHash {
		t.Fatalf("stored hash %q does not match hash of issued secret", repository.recordedCreate.ClientSecretHash)
	}
	if sink.revision != 4 {
		t.Fatalf("revision = %d, want 4", sink.revision)
	}
}

func TestCreateClientPublicNeverIssuesSecret(t *testing.T) {
	repository := &fakeOAuthStore{createResult: store.OAuthClientMutationResult{
		Client: store.OAuthClient{ID: uuid.New(), ClientType: store.OAuthClientPublic},
	}}
	service, _ := newTestService(t, repository)
	result, err := service.CreateClient(t.Context(), browserActor(t), CreateClientInput{
		Name: "Native app", ClientType: store.OAuthClientPublic,
		RedirectURIs: []string{"https://app.example.com/callback"}, AllowedScopes: []string{"service.read"},
	})
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if result.ClientSecret != "" || repository.recordedCreate.ClientSecretHash != "" {
		t.Fatal("public client must never receive or persist a secret")
	}
}

func TestRevokeClientPropagatesStoreErrorAndSkipsRevisionBump(t *testing.T) {
	repository := &fakeOAuthStore{revokeClientErr: store.ErrOAuthClientNotFound}
	service, sink := newTestService(t, repository)
	err := service.RevokeClient(t.Context(), browserActor(t), uuid.New())
	if !errors.Is(err, store.ErrOAuthClientNotFound) {
		t.Fatalf("RevokeClient error = %v, want ErrOAuthClientNotFound", err)
	}
	if sink.revision != 0 {
		t.Fatalf("revision sink updated on failed revoke: %d", sink.revision)
	}
}

func TestAuthorizeRequiresBrowserSessionActor(t *testing.T) {
	service, _ := newTestService(t, &fakeOAuthStore{})
	nonBrowser := accesscontrol.Actor{SubjectID: uuid.New(), CredentialSource: "api_key"}
	if _, err := service.Authorize(t.Context(), nonBrowser, AuthorizeRequest{}); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("Authorize error = %v, want ErrAuthenticationRequired", err)
	}
}

func TestAuthorizeRejectsUnknownClient(t *testing.T) {
	repository := &fakeOAuthStore{clientErr: store.ErrOAuthClientNotFound}
	service, _ := newTestService(t, repository)
	client := confidentialClient()
	if _, err := service.Authorize(t.Context(), browserActor(t, "service.read"), authorizeRequest(client)); !errors.Is(err, ErrUnauthorizedClient) {
		t.Fatalf("Authorize error = %v, want ErrUnauthorizedClient", err)
	}
}

func TestAuthorizeRejectsUnregisteredRedirectURI(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client}
	service, _ := newTestService(t, repository)
	req := authorizeRequest(client)
	req.RedirectURI = "https://attacker.example.com/callback"
	if _, err := service.Authorize(t.Context(), browserActor(t, "service.read"), req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Authorize error = %v, want ErrInvalidRequest", err)
	}
}

func TestAuthorizeRequiresPKCES256CodeChallenge(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client}
	service, _ := newTestService(t, repository)
	actor := browserActor(t, "service.read")
	cases := []struct {
		name   string
		mutate func(*AuthorizeRequest)
	}{
		{"missing challenge", func(r *AuthorizeRequest) { r.CodeChallenge = "" }},
		{"wrong method", func(r *AuthorizeRequest) { r.CodeChallengeMethod = "plain" }},
		{"too short", func(r *AuthorizeRequest) { r.CodeChallenge = "short" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			req := authorizeRequest(client)
			testCase.mutate(&req)
			if _, err := service.Authorize(t.Context(), actor, req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Authorize error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestAuthorizeRejectsScopeNotRegisteredForClient(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client}
	service, _ := newTestService(t, repository)
	req := authorizeRequest(client)
	req.Scope = []string{"account.manage"} // valid permission, but not in client.AllowedScopes
	if _, err := service.Authorize(t.Context(), browserActor(t, "account.manage"), req); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("Authorize error = %v, want ErrInvalidScope", err)
	}
}

func TestAuthorizeRejectsScopeExceedingActorPermission(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client}
	service, _ := newTestService(t, repository)
	// The actor never granted service.read at all, so even though the client
	// allows it, the user's own live permissions form the ceiling.
	actor := browserActor(t)
	if _, err := service.Authorize(t.Context(), actor, authorizeRequest(client)); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Authorize error = %v, want ErrAccessDenied", err)
	}
}

func TestAuthorizeRequiresConsentWhenNonePreviouslyGranted(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client, consentFound: false}
	service, _ := newTestService(t, repository)
	result, err := service.Authorize(t.Context(), browserActor(t, "service.read"), authorizeRequest(client))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !result.RequiresConsent {
		t.Fatal("expected consent to be required for a first-time authorization")
	}
	if repository.recordedCode.ID != uuid.Nil {
		t.Fatal("no authorization code should be issued before consent")
	}
}

func TestAuthorizeSkipsConsentWhenAlreadyGrantedSuperset(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{
		client: client, consentFound: true,
		consent: store.OAuthConsent{GrantedScope: []string{"service.read", "service.consume"}},
	}
	service, sink := newTestService(t, repository)
	req := authorizeRequest(client)
	result, err := service.Authorize(t.Context(), browserActor(t, "service.read"), req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if result.RequiresConsent {
		t.Fatal("expected consent to be skipped when already granted at or above the requested scope")
	}
	if !strings.Contains(result.RedirectURL, "code=") || !strings.Contains(result.RedirectURL, "state=xyz") {
		t.Fatalf("redirect URL missing code/state: %s", result.RedirectURL)
	}
	if repository.recordedCode.ClientID != client.ID {
		t.Fatal("authorization code was not recorded against the resolved client")
	}
	_ = sink
}

func TestConsentIssuesCodeAndRedirectsWithState(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client}
	service, _ := newTestService(t, repository)
	actor := browserActor(t, "service.read")
	redirectURL, err := service.Consent(t.Context(), actor, ConsentRequest{
		ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0], Scope: []string{"service.read"},
		State: "abc", CodeChallenge: validCodeChallenge, CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("Consent: %v", err)
	}
	if !strings.Contains(redirectURL, "code=") || !strings.Contains(redirectURL, "state=abc") {
		t.Fatalf("redirect URL missing code/state: %s", redirectURL)
	}
	if repository.recordedConsent.SubjectID != actor.SubjectID {
		t.Fatal("consent was not recorded against the authorizing user")
	}
}

func TestConsentRequiresBrowserSessionActor(t *testing.T) {
	service, _ := newTestService(t, &fakeOAuthStore{})
	nonBrowser := accesscontrol.Actor{SubjectID: uuid.New(), CredentialSource: "oauth_client"}
	if _, err := service.Consent(t.Context(), nonBrowser, ConsentRequest{}); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("Consent error = %v, want ErrAuthenticationRequired", err)
	}
}

func TestTokenAuthorizationCodeGrantIssuesTokenPair(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{
		client: client, clientSecret: hashSecret("correct-secret"),
		exchangeResult: store.OAuthTokenMetadata{Scope: []string{"service.read"}, AuthorizationRevision: 9},
	}
	service, sink := newTestService(t, repository)
	response, err := service.Token(t.Context(), TokenRequest{
		GrantType: "authorization_code", Code: "raw-code", RedirectURI: client.RedirectURIs[0],
		CodeVerifier: strings.Repeat("v", 43), ClientID: client.ClientID, ClientSecret: "correct-secret",
	})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" || response.TokenType != "Bearer" {
		t.Fatalf("unexpected token response: %#v", response)
	}
	if repository.recordedExchange.CodeHash != hashSecret("raw-code") {
		t.Fatal("code was not hashed before reaching the store")
	}
	if sink.revision != 9 {
		t.Fatalf("revision = %d, want 9", sink.revision)
	}
}

func TestTokenRefreshGrantRotatesAndReportsRevision(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{
		client: client, clientSecret: hashSecret("correct-secret"),
		rotateResult: store.OAuthTokenMetadata{Scope: []string{"service.read"}, AuthorizationRevision: 11},
	}
	service, sink := newTestService(t, repository)
	response, err := service.Token(t.Context(), TokenRequest{
		GrantType: "refresh_token", RefreshToken: "raw-refresh", ClientID: client.ClientID, ClientSecret: "correct-secret",
	})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" {
		t.Fatal("refresh grant did not issue a fresh token pair")
	}
	if repository.recordedRotate.RefreshTokenHash != hashSecret("raw-refresh") {
		t.Fatal("refresh token was not hashed before reaching the store")
	}
	if sink.revision != 11 {
		t.Fatalf("revision = %d, want 11", sink.revision)
	}
}

func TestTokenTranslatesStoreGrantErrorsToInvalidGrant(t *testing.T) {
	client := confidentialClient()
	for _, storeErr := range []error{store.ErrOAuthGrantDenied, store.ErrOAuthGrantReuseDetected} {
		repository := &fakeOAuthStore{client: client, clientSecret: hashSecret("correct-secret"), exchangeErr: storeErr}
		service, _ := newTestService(t, repository)
		_, err := service.Token(t.Context(), TokenRequest{
			GrantType: "authorization_code", Code: "c", RedirectURI: client.RedirectURIs[0],
			CodeVerifier: strings.Repeat("v", 43), ClientID: client.ClientID, ClientSecret: "correct-secret",
		})
		if !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("Token error = %v, want ErrInvalidGrant (from %v)", err, storeErr)
		}
	}
}

func TestTokenRejectsUnsupportedGrantType(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client, clientSecret: hashSecret("correct-secret")}
	service, _ := newTestService(t, repository)
	_, err := service.Token(t.Context(), TokenRequest{GrantType: "password", ClientID: client.ClientID, ClientSecret: "correct-secret"})
	if !errors.Is(err, ErrUnsupportedGrantType) {
		t.Fatalf("Token error = %v, want ErrUnsupportedGrantType", err)
	}
}

func TestTokenRejectsWrongConfidentialClientSecret(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client, clientSecret: hashSecret("correct-secret")}
	service, _ := newTestService(t, repository)
	_, err := service.Token(t.Context(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: client.RedirectURIs[0],
		CodeVerifier: strings.Repeat("v", 43), ClientID: client.ClientID, ClientSecret: "wrong-secret",
	})
	if !errors.Is(err, ErrUnauthorizedClient) {
		t.Fatalf("Token error = %v, want ErrUnauthorizedClient", err)
	}
}

func TestTokenPublicClientSkipsSecretCheck(t *testing.T) {
	client := confidentialClient()
	client.ClientType = store.OAuthClientPublic
	repository := &fakeOAuthStore{client: client, exchangeResult: store.OAuthTokenMetadata{Scope: []string{"service.read"}}}
	service, _ := newTestService(t, repository)
	if _, err := service.Token(t.Context(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: client.RedirectURIs[0],
		CodeVerifier: strings.Repeat("v", 43), ClientID: client.ClientID,
	}); err != nil {
		t.Fatalf("Token: %v", err)
	}
}

func TestRevokeAlwaysSucceedsRegardlessOfTokenExistence(t *testing.T) {
	client := confidentialClient()
	// RFC 7009: revocation is idempotent and never reveals whether the
	// presented token existed, so the service must not surface revokeFound.
	repository := &fakeOAuthStore{client: client, clientSecret: hashSecret("correct-secret"), revokeFound: false}
	service, _ := newTestService(t, repository)
	err := service.Revoke(t.Context(), RevokeRequest{Token: "whatever", ClientID: client.ClientID, ClientSecret: "correct-secret"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

func TestRevokeRejectsMissingToken(t *testing.T) {
	service, _ := newTestService(t, &fakeOAuthStore{})
	if err := service.Revoke(t.Context(), RevokeRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Revoke error = %v, want ErrInvalidRequest", err)
	}
}

func TestConnectedAppsRequireBrowserSessionActor(t *testing.T) {
	service, _ := newTestService(t, &fakeOAuthStore{})
	nonBrowser := accesscontrol.Actor{SubjectID: uuid.New(), CredentialSource: "oauth_client"}
	if _, err := service.ListConnectedApps(t.Context(), nonBrowser); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("ListConnectedApps error = %v, want ErrAuthenticationRequired", err)
	}
	if err := service.RevokeConnectedApp(t.Context(), nonBrowser, uuid.New()); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("RevokeConnectedApp error = %v, want ErrAuthenticationRequired", err)
	}
}

func TestListConnectedAppsReturnsStoreResults(t *testing.T) {
	apps := []store.OAuthConnectedApp{{ClientID: uuid.New(), ClientName: "Slack"}}
	repository := &fakeOAuthStore{connectedApps: apps}
	service, _ := newTestService(t, repository)
	got, err := service.ListConnectedApps(t.Context(), browserActor(t))
	if err != nil || len(got) != 1 || got[0].ClientName != "Slack" {
		t.Fatalf("ListConnectedApps = %#v, %v", got, err)
	}
}

func TestIsOAuthClientActorMatchesOnlyDelegatedTokens(t *testing.T) {
	if IsOAuthClientActor(accesscontrol.Actor{CredentialSource: "managed_login"}) {
		t.Fatal("managed_login incorrectly classified as an OAuth client actor")
	}
	if !IsOAuthClientActor(accesscontrol.Actor{CredentialSource: "oauth_client"}) {
		t.Fatal("oauth_client actor not recognized")
	}
}
