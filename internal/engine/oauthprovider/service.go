// Package oauthprovider implements Engine as an OAuth2 authorization server
// for third-party clients: an external application can obtain a delegated,
// scope-limited access token representing one specific Fused workspace user
// via a standard Authorization Code + PKCE flow, without either the end user
// or the third party ever needing fused-cli. This is the inverse of the
// existing hosted-connect flow, where Fused brokers a workspace's OAuth
// connection TO a third-party provider.
package oauthprovider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultAccessTokenTTL  = time.Hour
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	authorizationCodeTTL   = 60 * time.Second

	clientIDPrefix     = "foc_"
	clientSecretPrefix = "fos_"
	accessTokenPrefix  = "foat_"
	refreshTokenPrefix = "fort_"
)

var (
	ErrInvalidRequest       = errors.New("invalid OAuth request")
	ErrUnauthorizedClient   = errors.New("unauthorized OAuth client")
	ErrAccessDenied         = errors.New("OAuth scope exceeds the authorizing user's own permissions")
	ErrUnsupportedGrantType = errors.New("unsupported OAuth grant type")
	ErrInvalidGrant         = errors.New("invalid OAuth grant")
	ErrInvalidScope         = errors.New("invalid OAuth scope")
)

// IsOAuthClientActor mirrors browserauth.IsBrowserSessionActor: it tells
// downstream code (route carve-outs, admin-only endpoint guards) whether an
// authenticated actor is a delegated third-party token rather than a Fused
// user's own credential, so those paths can be excluded regardless of the
// token's claimed scope.
func IsOAuthClientActor(actor accesscontrol.Actor) bool {
	return actor.CredentialSource == "oauth_client"
}

type RevisionSink interface {
	SetRevision(int64) bool
}

type Service struct {
	store      store.OAuthClientStore
	revisions  RevisionSink
	now        func() time.Time
	accessTTL  time.Duration
	refreshTTL time.Duration
	codeTTL    time.Duration
}

func NewService(repository store.OAuthClientStore, revisions RevisionSink) (*Service, error) {
	if repository == nil || revisions == nil {
		return nil, errors.New("invalid OAuth provider configuration")
	}
	return &Service{
		store: repository, revisions: revisions, now: time.Now,
		accessTTL: defaultAccessTokenTTL, refreshTTL: defaultRefreshTokenTTL, codeTTL: authorizationCodeTTL,
	}, nil
}

// -- Admin client management (Owner/Admin only; enforced by the caller) --

type CreateClientInput struct {
	Name          string
	ClientType    store.OAuthClientType
	RedirectURIs  []string
	AllowedScopes []string
}

type CreateClientResult struct {
	Client       store.OAuthClient
	ClientSecret string
}

func (s *Service) CreateClient(ctx context.Context, actor accesscontrol.Actor, input CreateClientInput) (CreateClientResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.client.create")
	defer span.End()
	span.SetAttributes(attribute.String("oauth.client_type", string(input.ClientType)))
	clientID, err := randomIdentifier(clientIDPrefix, 16)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return CreateClientResult{}, err
	}
	registration := store.OAuthClientRegistration{
		Name: input.Name, ClientType: input.ClientType, ClientID: clientID,
		RedirectURIs: input.RedirectURIs, AllowedScopes: input.AllowedScopes,
		Actor: mutationActor(ctx, actor),
	}
	var rawSecret string
	if input.ClientType == store.OAuthClientConfidential {
		rawSecret, err = randomIdentifier(clientSecretPrefix, 32)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "failed"))
			return CreateClientResult{}, err
		}
		registration.ClientSecretHash = hashSecret(rawSecret)
	}
	result, err := s.store.CreateOAuthClient(ctx, registration)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return CreateClientResult{}, err
	}
	s.revisions.SetRevision(result.AuthorizationRevision)
	span.SetAttributes(attribute.String("outcome", "created"), attribute.String("oauth.client_id", result.Client.ClientID))
	return CreateClientResult{Client: result.Client, ClientSecret: rawSecret}, nil
}

func (s *Service) ListClients(ctx context.Context) ([]store.OAuthClient, error) {
	return s.store.ListOAuthClients(ctx)
}

func (s *Service) RevokeClient(ctx context.Context, actor accesscontrol.Actor, id uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.client.revoke")
	defer span.End()
	revision, err := s.store.RevokeOAuthClient(ctx, id, mutationActor(ctx, actor))
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return err
	}
	s.revisions.SetRevision(revision)
	span.SetAttributes(attribute.String("outcome", "revoked"))
	return nil
}

// -- Authorization Code + PKCE flow (browser-session actor required) --

type AuthorizeRequest struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scope               []string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

type AuthorizeResult struct {
	RequiresConsent bool
	RedirectURL     string
	Client          store.OAuthClient
	Scope           []string
}

func (s *Service) Authorize(ctx context.Context, actor accesscontrol.Actor, req AuthorizeRequest) (AuthorizeResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.authorize")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"))
	if !browserauth.IsBrowserSessionActor(actor) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return AuthorizeResult{}, accesscontrol.ErrAuthenticationRequired
	}
	client, err := s.resolveAuthorizingClient(ctx, req)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return AuthorizeResult{}, err
	}
	scope, err := s.resolveGrantableScope(ctx, actor, client, req.Scope)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return AuthorizeResult{}, err
	}
	consent, found, err := s.store.GetOAuthUserConsent(ctx, client.ID, actor.SubjectID)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return AuthorizeResult{}, err
	}
	if !found || !scopeSubset(scope, consent.GrantedScope) {
		span.SetAttributes(attribute.String("outcome", "consent_required"))
		return AuthorizeResult{RequiresConsent: true, Client: client, Scope: scope}, nil
	}
	redirectURL, err := s.grantConsentAndIssueCode(ctx, actor, client, scope, req.RedirectURI, req.CodeChallenge, req.State)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return AuthorizeResult{}, err
	}
	span.SetAttributes(attribute.String("outcome", "consent_skipped"))
	return AuthorizeResult{RequiresConsent: false, RedirectURL: redirectURL, Client: client, Scope: scope}, nil
}

type ConsentRequest struct {
	ClientID            string
	RedirectURI         string
	Scope               []string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Consent is the CSRF-protected POST that runs after Authorize returns
// RequiresConsent: true. It re-validates the client/redirect_uri/PKCE shape
// from scratch rather than trusting anything about the prior Authorize call,
// since the consent screen round-trips through the end user's browser.
func (s *Service) Consent(ctx context.Context, actor accesscontrol.Actor, req ConsentRequest) (string, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.consent")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"))
	if !browserauth.IsBrowserSessionActor(actor) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return "", accesscontrol.ErrAuthenticationRequired
	}
	client, err := s.resolveAuthorizingClient(ctx, AuthorizeRequest{
		ClientID: req.ClientID, RedirectURI: req.RedirectURI, ResponseType: "code",
		CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod,
	})
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return "", err
	}
	scope, err := s.resolveGrantableScope(ctx, actor, client, req.Scope)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return "", err
	}
	redirectURL, err := s.grantConsentAndIssueCode(ctx, actor, client, scope, req.RedirectURI, req.CodeChallenge, req.State)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return "", err
	}
	span.SetAttributes(attribute.String("outcome", "consented"))
	return redirectURL, nil
}

func (s *Service) grantConsentAndIssueCode(ctx context.Context, actor accesscontrol.Actor, client store.OAuthClient, scope []string, redirectURI, codeChallenge, state string) (string, error) {
	rawCode, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	err = s.store.RecordOAuthConsentAndIssueCode(ctx,
		store.OAuthConsentGrant{ClientID: client.ID, SubjectID: actor.SubjectID, GrantedScope: scope},
		store.OAuthAuthorizationCodeIssue{
			ID: uuid.New(), ClientID: client.ID, SubjectID: actor.SubjectID, RedirectURI: redirectURI,
			Scope: scope, CodeHash: hashSecret(rawCode), CodeChallenge: codeChallenge,
			ExpiresAt: now.Add(s.codeTTL),
		},
	)
	if err != nil {
		return "", err
	}
	return buildRedirectURL(redirectURI, rawCode, state)
}

func (s *Service) resolveAuthorizingClient(ctx context.Context, req AuthorizeRequest) (store.OAuthClient, error) {
	if req.ResponseType != "code" {
		return store.OAuthClient{}, fmt.Errorf("%w: response_type must be code", ErrInvalidRequest)
	}
	if req.CodeChallengeMethod != "S256" || len(req.CodeChallenge) < 43 || len(req.CodeChallenge) > 128 {
		return store.OAuthClient{}, fmt.Errorf("%w: PKCE S256 code_challenge is required", ErrInvalidRequest)
	}
	client, _, err := s.store.GetOAuthClientByPublicID(ctx, req.ClientID)
	if err != nil {
		return store.OAuthClient{}, ErrUnauthorizedClient
	}
	if !exactRedirectURIMatch(client.RedirectURIs, req.RedirectURI) {
		return store.OAuthClient{}, fmt.Errorf("%w: redirect_uri is not registered for this client", ErrInvalidRequest)
	}
	return client, nil
}

// resolveGrantableScope enforces the scope ceiling: every requested scope
// must be both registered for the client AND held by the authorizing user
// right now (checked against their live AuthorizationSnapshot, not a value
// cached at some earlier time). The same ceiling is re-verified on every
// subsequent API call via loadOAuthTokenPrincipal, so a permission the user
// later loses stops being usable through the token immediately -- this call
// only decides what a fresh consent may grant.
func (s *Service) resolveGrantableScope(ctx context.Context, actor accesscontrol.Actor, client store.OAuthClient, requested []string) ([]string, error) {
	scope := requested
	if len(scope) == 0 {
		scope = client.AllowedScopes
	}
	allowed := make(map[string]struct{}, len(client.AllowedScopes))
	for _, item := range client.AllowedScopes {
		allowed[item] = struct{}{}
	}
	authorizer := accesscontrol.SnapshotAuthorizer{}
	granted := make([]string, 0, len(scope))
	for _, item := range scope {
		permission := accesscontrol.Permission(item)
		if err := accesscontrol.ValidatePermission(permission); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidScope, item)
		}
		if _, ok := allowed[item]; !ok {
			return nil, fmt.Errorf("%w: %q is not registered for this client", ErrInvalidScope, item)
		}
		if !actorHoldsPermission(ctx, authorizer, actor, permission) {
			return nil, fmt.Errorf("%w: %q", ErrAccessDenied, item)
		}
		granted = append(granted, item)
	}
	if len(granted) == 0 {
		return nil, fmt.Errorf("%w: no grantable scope", ErrInvalidScope)
	}
	return granted, nil
}

func actorHoldsPermission(ctx context.Context, authorizer accesscontrol.SnapshotAuthorizer, actor accesscontrol.Actor, permission accesscontrol.Permission) bool {
	for _, resourceType := range accesscontrol.GrantableResourceTypes(permission) {
		scope, err := authorizer.Scope(ctx, actor, permission, resourceType)
		if err == nil && (scope.All || len(scope.IDs) > 0) {
			return true
		}
	}
	return false
}

func scopeSubset(requested, granted []string) bool {
	grantedSet := make(map[string]struct{}, len(granted))
	for _, item := range granted {
		grantedSet[item] = struct{}{}
	}
	for _, item := range requested {
		if _, ok := grantedSet[item]; !ok {
			return false
		}
	}
	return true
}

func exactRedirectURIMatch(registered []string, candidate string) bool {
	for _, uri := range registered {
		if uri == candidate {
			return true
		}
	}
	return false
}

func buildRedirectURL(redirectURI, code, state string) (string, error) {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("%w: invalid redirect_uri", ErrInvalidRequest)
	}
	query := parsed.Query()
	query.Set("code", code)
	if state != "" {
		query.Set("state", state)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// -- Token endpoint: authorization_code and refresh_token grants --

type TokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	CodeVerifier string
	RefreshToken string
	ClientID     string
	ClientSecret string
}

type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int64
	Scope        []string
}

func (s *Service) Token(ctx context.Context, req TokenRequest) (TokenResponse, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.token")
	defer span.End()
	span.SetAttributes(attribute.String("oauth.grant_type", req.GrantType))
	client, err := s.authenticateClient(ctx, req.ClientID, req.ClientSecret)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return TokenResponse{}, err
	}
	var response TokenResponse
	switch req.GrantType {
	case "authorization_code":
		response, err = s.exchangeAuthorizationCode(ctx, client, req)
	case "refresh_token":
		response, err = s.exchangeRefreshToken(ctx, client, req)
	default:
		err = ErrUnsupportedGrantType
	}
	if err != nil {
		span.SetAttributes(attribute.String("outcome", tokenOutcome(err)))
		return TokenResponse{}, err
	}
	span.SetAttributes(attribute.String("outcome", "issued"))
	return response, nil
}

func (s *Service) exchangeAuthorizationCode(ctx context.Context, client store.OAuthClient, req TokenRequest) (TokenResponse, error) {
	if req.Code == "" || req.RedirectURI == "" || req.CodeVerifier == "" {
		return TokenResponse{}, ErrInvalidRequest
	}
	issue, rawAccess, rawRefresh, err := s.newTokenIssue()
	if err != nil {
		return TokenResponse{}, err
	}
	metadata, err := s.store.ExchangeOAuthAuthorizationCode(ctx, store.OAuthCodeExchange{
		CodeHash: hashSecret(req.Code), ClientID: client.ID, RedirectURI: req.RedirectURI,
		CodeVerifier: req.CodeVerifier, Issue: issue,
	}, s.now().UTC())
	if err != nil {
		return TokenResponse{}, translateGrantError(err)
	}
	s.revisions.SetRevision(metadata.AuthorizationRevision)
	return tokenResponse(metadata, rawAccess, rawRefresh, s.accessTTL), nil
}

func (s *Service) exchangeRefreshToken(ctx context.Context, client store.OAuthClient, req TokenRequest) (TokenResponse, error) {
	if req.RefreshToken == "" {
		return TokenResponse{}, ErrInvalidRequest
	}
	issue, rawAccess, rawRefresh, err := s.newTokenIssue()
	if err != nil {
		return TokenResponse{}, err
	}
	metadata, err := s.store.RotateOAuthRefreshToken(ctx, store.OAuthRefreshExchange{
		RefreshTokenHash: hashSecret(req.RefreshToken), ClientID: client.ID, Issue: issue,
	}, s.now().UTC())
	if err != nil {
		// A reuse-detected cascade revoke already bumped the DB revision inside
		// the store transaction; this process picks it up on its next poll tick
		// (see accesscontrol.PollAuthorizationRevisions) rather than synchronously
		// here, since the failed exchange has no fresh revision value to report.
		return TokenResponse{}, translateGrantError(err)
	}
	s.revisions.SetRevision(metadata.AuthorizationRevision)
	return tokenResponse(metadata, rawAccess, rawRefresh, s.accessTTL), nil
}

func tokenResponse(metadata store.OAuthTokenMetadata, rawAccess, rawRefresh string, accessTTL time.Duration) TokenResponse {
	return TokenResponse{
		AccessToken: rawAccess, RefreshToken: rawRefresh, TokenType: "Bearer",
		ExpiresIn: int64(accessTTL.Seconds()), Scope: metadata.Scope,
	}
}

func (s *Service) newTokenIssue() (store.OAuthTokenIssue, string, string, error) {
	rawAccess, err := randomIdentifier(accessTokenPrefix, 32)
	if err != nil {
		return store.OAuthTokenIssue{}, "", "", err
	}
	rawRefresh, err := randomIdentifier(refreshTokenPrefix, 32)
	if err != nil {
		return store.OAuthTokenIssue{}, "", "", err
	}
	now := time.Now().UTC()
	issue := store.OAuthTokenIssue{
		AccessTokenHash: hashSecret(rawAccess), RefreshTokenHash: hashSecret(rawRefresh),
		AccessExpiresAt: now.Add(s.accessTTL), RefreshExpiresAt: now.Add(s.refreshTTL),
	}
	return issue, rawAccess, rawRefresh, nil
}

func (s *Service) authenticateClient(ctx context.Context, clientID, clientSecret string) (store.OAuthClient, error) {
	client, secretHash, err := s.store.GetOAuthClientByPublicID(ctx, clientID)
	if err != nil {
		return store.OAuthClient{}, ErrUnauthorizedClient
	}
	if client.ClientType == store.OAuthClientConfidential {
		if clientSecret == "" || secretHash == "" ||
			subtle.ConstantTimeCompare([]byte(hashSecret(clientSecret)), []byte(secretHash)) != 1 {
			return store.OAuthClient{}, ErrUnauthorizedClient
		}
	}
	return client, nil
}

func translateGrantError(err error) error {
	if errors.Is(err, store.ErrOAuthGrantDenied) || errors.Is(err, store.ErrOAuthGrantReuseDetected) {
		return ErrInvalidGrant
	}
	return err
}

func tokenOutcome(err error) string {
	if errors.Is(err, ErrInvalidGrant) || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrUnsupportedGrantType) {
		return "denied"
	}
	return "failed"
}

// -- RFC 7009 client-initiated revocation --

type RevokeRequest struct {
	Token        string
	ClientID     string
	ClientSecret string
}

func (s *Service) Revoke(ctx context.Context, req RevokeRequest) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.revoke")
	defer span.End()
	if req.Token == "" {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return ErrInvalidRequest
	}
	client, err := s.authenticateClient(ctx, req.ClientID, req.ClientSecret)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return err
	}
	revoked, err := s.store.RevokeOAuthTokenByHash(ctx, client.ID, hashSecret(req.Token))
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return err
	}
	span.SetAttributes(attribute.Bool("oauth.token_found", revoked), attribute.String("outcome", "processed"))
	return nil
}

// -- End-user self-service: "Connected apps" --

func (s *Service) ListConnectedApps(ctx context.Context, actor accesscontrol.Actor) ([]store.OAuthConnectedApp, error) {
	if !browserauth.IsBrowserSessionActor(actor) {
		return nil, accesscontrol.ErrAuthenticationRequired
	}
	return s.store.ListOAuthConnectedApps(ctx, actor.SubjectID)
}

func (s *Service) RevokeConnectedApp(ctx context.Context, actor accesscontrol.Actor, clientID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.connected_app.revoke")
	defer span.End()
	if !browserauth.IsBrowserSessionActor(actor) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return accesscontrol.ErrAuthenticationRequired
	}
	revision, err := s.store.RevokeOAuthConnectedApp(ctx, clientID, actor.SubjectID, mutationActor(ctx, actor))
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return err
	}
	s.revisions.SetRevision(revision)
	span.SetAttributes(attribute.String("outcome", "revoked"))
	return nil
}

// -- Background cleanup --

func (s *Service) StartCleanupWorker(ctx context.Context, interval time.Duration) {
	if interval < 10*time.Second || interval > time.Hour {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.store.ExpireOAuthArtifacts(ctx, s.now().UTC(), 500)
			}
		}
	}()
}

// -- shared crypto/actor helpers --

func mutationActor(ctx context.Context, actor accesscontrol.Actor) store.MutationActor {
	return store.MutationActor{
		SubjectID: actor.SubjectID, CredentialID: actor.CredentialID,
		RequestID: middleware.GetReqID(ctx), TraceID: traceID(ctx),
	}
}

func traceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

func randomToken(byteLength int) (string, error) {
	buf := make([]byte, byteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomIdentifier(prefix string, byteLength int) (string, error) {
	token, err := randomToken(byteLength)
	if err != nil {
		return "", err
	}
	return prefix + token, nil
}

func hashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
