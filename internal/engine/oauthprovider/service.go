// Package oauthprovider adapts the shared OAuth server to Fused identities,
// permission snapshots, and expiry-aware PostgreSQL persistence.
package oauthprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/fused-open-core/oauthserver"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

const (
	clientIDPrefix     = "foc_"
	clientSecretPrefix = "fos_"
	accessTokenPrefix  = "foat_"
	refreshTokenPrefix = "fort_"
)

var (
	ErrInvalidRequest       = oauthserver.ErrInvalidRequest
	ErrUnauthorizedClient   = oauthserver.ErrUnauthorizedClient
	ErrAccessDenied         = oauthserver.ErrAccessDenied
	ErrUnsupportedGrantType = oauthserver.ErrUnsupportedGrantType
	ErrInvalidGrant         = oauthserver.ErrInvalidGrant
	ErrInvalidScope         = oauthserver.ErrInvalidScope
)

type RevisionSink = oauthserver.RevisionSink
type CreateClientInput = oauthserver.CreateClientInput
type AuthorizeRequest = oauthserver.AuthorizeRequest
type ConsentRequest = oauthserver.ConsentRequest
type TokenRequest = oauthserver.TokenRequest
type TokenResponse = oauthserver.TokenResponse
type RevokeRequest = oauthserver.RevokeRequest

// Fused responses retain the host model's temporary-client expiry metadata.
type CreateClientResult struct {
	Client       store.OAuthClient
	ClientSecret string
}
type AuthorizeResult struct {
	RequiresConsent bool
	RedirectURL     string
	Client          store.OAuthClient
	Scope           []string
}

type scopeCatalog struct{}

// ValidateScope lets the protocol core use Fused's canonical permission vocabulary.
func (scopeCatalog) ValidateScope(scope string) error {
	return accesscontrol.ValidatePermission(accesscontrol.Permission(scope))
}

type Service struct {
	core      *oauthserver.Service[uuid.UUID]
	store     store.OAuthClientStore
	revisions RevisionSink
	now       func() time.Time
}

// newCore configures the same shared server as Threadify with Fused's credential namespace.
func newCore(repository oauthserver.Store[uuid.UUID], revisions RevisionSink) (*oauthserver.Service[uuid.UUID], error) {
	return oauthserver.NewService(repository, revisions, scopeCatalog{}, oauthserver.Prefixes{
		ClientID: clientIDPrefix, ClientSecret: clientSecretPrefix,
		AccessToken: accessTokenPrefix, RefreshToken: refreshTokenPrefix,
	})
}

// NewService keeps Fused-only policies at the boundary while delegating OAuth protocol work.
func NewService(repository store.OAuthClientStore, revisions RevisionSink) (*Service, error) {
	// Reject missing storage before wrapping it in a non-nil adapter.
	if repository == nil || revisions == nil {
		return nil, errors.New("invalid OAuth provider configuration")
	}
	core, err := newCore(coreStore{repository}, revisions)
	// Constructor failures must not leave a partially configured provider.
	if err != nil {
		return nil, err
	}
	return &Service{core: core, store: repository, revisions: revisions, now: time.Now}, nil
}

// requestCore isolates persisted response metadata from concurrent protocol requests.
func (s *Service) requestCore() (*oauthserver.Service[uuid.UUID], *requestStore, error) {
	repository := &requestStore{coreStore: coreStore{s.store}}
	core, err := newCore(repository, s.revisions)
	return core, repository, err
}

// IsOAuthClientActor identifies delegated credentials for Fused's route restrictions.
func IsOAuthClientActor(actor accesscontrol.Actor) bool {
	return actor.CredentialSource == "oauth_client"
}

// adaptActor supplies live Fused permission checks without coupling the core to Engine RBAC.
func adaptActor(ctx context.Context, actor accesscontrol.Actor) oauthserver.Actor[uuid.UUID] {
	return oauthserver.Actor[uuid.UUID]{
		SubjectID: actor.SubjectID, CredentialID: actor.CredentialID,
		IsBrowserSession: browserauth.IsBrowserSessionActor(actor),
		RequestID:        middleware.GetReqID(ctx), TraceID: traceID(ctx),
		// Evaluate grants against each valid resource type, including resource-scoped roles.
		CanGrant: func(ctx context.Context, name string) (bool, error) {
			permission := accesscontrol.Permission(name)
			authorizer := accesscontrol.SnapshotAuthorizer{}
			// Any supported resource grant can authorize a scope without implying workspace-wide access.
			for _, resourceType := range accesscontrol.GrantableResourceTypes(permission) {
				scope, err := authorizer.Scope(ctx, actor, permission, resourceType)
				// Authorization failures never count as a grant.
				if err == nil && (scope.All || len(scope.IDs) > 0) {
					return true, nil
				}
			}
			return false, nil
		},
	}
}

// authenticationError retains the error identity consumed by Fused's browser-login handlers.
func authenticationError(err error) error {
	// The core has no dependency on Engine's authentication package.
	if errors.Is(err, oauthserver.ErrAuthenticationRequired) {
		return accesscontrol.ErrAuthenticationRequired
	}
	return err
}

// CreateClient uses shared secret issuance while returning the complete persisted Fused model.
func (s *Service) CreateClient(ctx context.Context, actor accesscontrol.Actor, input CreateClientInput) (CreateClientResult, error) {
	core, repository, err := s.requestCore()
	// No registration may proceed without a valid protocol adapter.
	if err != nil {
		return CreateClientResult{}, err
	}
	result, err := core.CreateClient(ctx, adaptActor(ctx, actor), input)
	// Failed registration must not expose credentials or a partial client projection.
	if err != nil {
		return CreateClientResult{}, err
	}
	return CreateClientResult{Client: repository.client, ClientSecret: result.ClientSecret}, nil
}

// ListClients preserves temporary-client expiry information absent from the core's older model.
func (s *Service) ListClients(ctx context.Context) ([]store.OAuthClient, error) {
	return s.store.ListOAuthClients(ctx)
}

// RevokeClient delegates revision propagation and protocol auditing to the shared server.
func (s *Service) RevokeClient(ctx context.Context, actor accesscontrol.Actor, id uuid.UUID) error {
	return s.core.RevokeClient(ctx, adaptActor(ctx, actor), id)
}

// Authorize requires explicit consent scopes before the shared core validates and issues grants.
func (s *Service) Authorize(ctx context.Context, actor accesscontrol.Actor, req AuthorizeRequest) (AuthorizeResult, error) {
	// Fused must never default an omitted request to every scope registered by the client.
	if browserauth.IsBrowserSessionActor(actor) && len(req.Scope) == 0 {
		return AuthorizeResult{}, fmt.Errorf("%w: explicit scope is required", ErrInvalidScope)
	}
	core, repository, err := s.requestCore()
	// Fail before evaluating grants if the adapter cannot be constructed.
	if err != nil {
		return AuthorizeResult{}, err
	}
	result, err := core.Authorize(ctx, adaptActor(ctx, actor), req)
	// Browser handlers rely on Fused's authentication error identity.
	if err != nil {
		return AuthorizeResult{}, authenticationError(err)
	}
	return AuthorizeResult{RequiresConsent: result.RequiresConsent, RedirectURL: result.RedirectURL, Client: repository.client, Scope: result.Scope}, nil
}

// Consent rechecks Fused's explicit-scope policy on the browser's consent submission.
func (s *Service) Consent(ctx context.Context, actor accesscontrol.Actor, req ConsentRequest) (string, error) {
	// A crafted consent POST must not bypass the authorize endpoint's scope ceiling.
	if browserauth.IsBrowserSessionActor(actor) && len(req.Scope) == 0 {
		return "", fmt.Errorf("%w: explicit scope is required", ErrInvalidScope)
	}
	result, err := s.core.Consent(ctx, adaptActor(ctx, actor), req)
	return result, authenticationError(err)
}

// Token delegates authentication and grant exchange, then advertises the persisted lifetime.
func (s *Service) Token(ctx context.Context, req TokenRequest) (TokenResponse, error) {
	core, repository, err := s.requestCore()
	// A request-local adapter is required to avoid leaking another grant's deadline.
	if err != nil {
		return TokenResponse{}, err
	}
	response, err := core.Token(ctx, req)
	// Denied grants must not return token material or synthesize a successful response.
	if err != nil {
		return TokenResponse{}, err
	}
	// Temporary clients cap the stored expiry; preserve all other shared-core response fields.
	response.ExpiresIn = max(0, int64(repository.metadata.AccessExpiresAt.Sub(s.now().UTC()).Seconds()))
	return response, nil
}

// Revoke uses shared client authentication and RFC 7009 idempotency semantics.
func (s *Service) Revoke(ctx context.Context, req RevokeRequest) error {
	return s.core.Revoke(ctx, req)
}

// ListConnectedApps preserves the browser-session gate implemented by the shared core.
func (s *Service) ListConnectedApps(ctx context.Context, actor accesscontrol.Actor) ([]store.OAuthConnectedApp, error) {
	result, err := s.core.ListConnectedApps(ctx, adaptActor(ctx, actor))
	return result, authenticationError(err)
}

// RevokeConnectedApp delegates consent/token revocation with Fused's audit identity.
func (s *Service) RevokeConnectedApp(ctx context.Context, actor accesscontrol.Actor, clientID uuid.UUID) error {
	return authenticationError(s.core.RevokeConnectedApp(ctx, adaptActor(ctx, actor), clientID))
}

// StartCleanupWorker lets the shared lifecycle expire Fused's durable OAuth artifacts.
func (s *Service) StartCleanupWorker(ctx context.Context, interval time.Duration) {
	s.core.StartCleanupWorker(ctx, interval)
}

// traceID omits absent traces rather than recording an invalid identifier.
func traceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	// Requests without an active trace have no audit trace identifier.
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}
