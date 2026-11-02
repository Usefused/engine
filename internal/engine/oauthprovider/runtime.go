package oauthprovider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/fused-open-core/oauthserver"
	"github.com/google/uuid"
)

// RevokeRuntime authenticates the OAuth client before retiring its independent execution credential.
func (s *Service) RevokeRuntime(ctx context.Context, req RevokeRequest) error {
	core, _, err := s.runtimeCore()
	// Client authentication must remain owned by the shared OAuth protocol implementation.
	if err != nil {
		return err
	}
	// The unknown-token path still validates confidential client credentials without leaking token existence.
	if err := core.Revoke(ctx, req); err != nil {
		return err
	}
	repository, ok := s.store.(store.OAuthRuntimeRevoker)
	// Unsupported backing cannot turn a revocation into an apparent success.
	if !ok {
		return errors.New("runtime revocation unavailable")
	}
	return repository.RevokeOAuthRuntimeToken(ctx, req.ClientID, hashSecret(req.Token))
}

// RuntimeTokenResponse keeps execution separate and optionally carries an explicitly consented controller-only deployment credential.
type RuntimeTokenResponse struct {
	store.OAuthRuntimeGrant
	RuntimeToken string `json:"runtime_token,omitempty"`
	*RuntimeManagementTokens
}

type runtimeStore struct{ *requestStore }

// GetOAuthUserConsent always requires review of the exact app manifest and selected buckets.
func (s runtimeStore) GetOAuthUserConsent(context.Context, uuid.UUID, uuid.UUID) (store.OAuthConsent, bool, error) {
	return store.OAuthConsent{}, false, nil
}

// GetOAuthClientByPublicID isolates provisioning clients from management and MCP issuers.
func (s runtimeStore) GetOAuthClientByPublicID(ctx context.Context, id string) (oauthserver.OAuthClient, string, error) {
	client, hash, err := s.requestStore.GetOAuthClientByPublicID(ctx, id)
	// A broader client must never exchange its authority for a runtime grant implicitly.
	if err != nil || !store.OAuthRuntimeOnly(client.AllowedScopes) {
		return oauthserver.OAuthClient{}, "", store.ErrOAuthClientNotFound
	}
	return client, hash, nil
}

// runtimeCore reuses shared PKCE, callback validation, expiry, and single-use exchange machinery.
func (s *Service) runtimeCore() (*oauthserver.Service[uuid.UUID], *requestStore, error) {
	repository := &requestStore{coreStore: coreStore{s.store}}
	core, err := newCore(runtimeStore{repository}, s.revisions)
	return core, repository, err
}

// runtimeActor exposes specialized protocol markers after app planning checks actual resource authority.
func runtimeActor(ctx context.Context, actor accesscontrol.Actor) oauthserver.Actor[uuid.UUID] {
	result := adaptActor(ctx, actor)
	// The marker is not an RBAC grant and cannot authorize any management endpoint.
	result.CanGrant = func(_ context.Context, scope string) (bool, error) {
		return scope == store.OAuthRuntimeScope || scope == store.OAuthDeploymentScope, nil
	}
	return result
}

// AuthorizeRuntime validates the browser's OAuth proof before exposing the registered manifest.
func (s *Service) AuthorizeRuntime(ctx context.Context, actor accesscontrol.Actor, req AuthorizeRequest) (AuthorizeResult, error) {
	// Explicit runtime-only scope avoids interpreting management permissions as provisioning consent.
	if !store.OAuthRuntimeOnly(req.Scope) {
		return AuthorizeResult{}, ErrInvalidScope
	}
	core, repository, err := s.runtimeCore()
	// Failed construction cannot produce an apparently valid consent page.
	if err != nil {
		return AuthorizeResult{}, err
	}
	result, err := core.Authorize(ctx, runtimeActor(ctx, actor), req)
	return AuthorizeResult{RequiresConsent: result.RequiresConsent, Client: repository.client, Scope: result.Scope}, authenticationError(err)
}

// ConsentRuntime binds one published API app to the authorization code transaction.
func (s *Service) ConsentRuntime(ctx context.Context, actor accesscontrol.Actor, req ConsentRequest, grant store.OAuthRuntimeGrant) (string, error) {
	// No alternate scope may gain a runtime app through the specialized issuer.
	if !store.OAuthRuntimeOnly(req.Scope) {
		return "", ErrInvalidScope
	}
	core, _, err := s.runtimeCore()
	// Storage/protocol initialization failure precedes all code issuance.
	if err != nil {
		return "", err
	}
	result, err := core.Consent(store.ContextWithRuntimeConsent(ctx, grant), runtimeActor(ctx, actor), req)
	return result, authenticationError(err)
}

// TokenRuntime exchanges a code once for a non-expiring app token pinned to its approved version.
func (s *Service) TokenRuntime(ctx context.Context, req TokenRequest) (RuntimeTokenResponse, error) {
	// Only app deployment grants may refresh management credentials; runtime tokens remain independent.
	if req.GrantType == "refresh_token" {
		return s.refreshRuntimeDeployment(ctx, req)
	}
	if req.GrantType != "authorization_code" {
		return RuntimeTokenResponse{}, ErrUnsupportedGrantType
	}
	core, repository, err := s.runtimeCore()
	// No independent credential is created when the protocol boundary is unavailable.
	if err != nil {
		return RuntimeTokenResponse{}, err
	}
	token, err := randomIdentifier("frt_", 32)
	// Entropy failures must not create predictable credentials.
	if err != nil {
		return RuntimeTokenResponse{}, err
	}
	exchange := &store.RuntimeExchange{TokenHash: hashSecret(token)}
	management, err := core.Token(store.ContextWithRuntimeExchange(ctx, exchange), req)
	// The shared transaction must commit before returning any runtime credential.
	if err != nil {
		return RuntimeTokenResponse{}, err
	}
	response := RuntimeTokenResponse{OAuthRuntimeGrant: exchange.Grant, RuntimeToken: token}
	// The management credential stays controller-only and exists only after explicit deployment consent.
	if len(management.Scope) == 1 && management.Scope[0] == store.OAuthDeploymentScope {
		management.ExpiresIn = max(0, int64(repository.metadata.AccessExpiresAt.Sub(s.now().UTC()).Seconds()))
		response.RuntimeManagementTokens = runtimeManagementTokens(management)
	}
	return response, nil
}

// refreshRuntimeDeployment rotates only controller credentials, preserving pinned execution tokens.
func (s *Service) refreshRuntimeDeployment(ctx context.Context, req TokenRequest) (RuntimeTokenResponse, error) {
	client, _, err := s.store.GetOAuthClientByPublicID(ctx, req.ClientID)
	// Execution-only clients cannot upgrade old consent into deployment management.
	if err != nil || len(client.AllowedScopes) != 1 || client.AllowedScopes[0] != store.OAuthDeploymentScope {
		return RuntimeTokenResponse{}, ErrUnsupportedGrantType
	}
	core, repository, err := s.runtimeCore()
	// Initialization failure must precede refresh-token consumption.
	if err != nil {
		return RuntimeTokenResponse{}, err
	}
	response, err := core.Token(ctx, req)
	// Revocation and reuse detection retain canonical OAuth errors.
	if err != nil {
		return RuntimeTokenResponse{}, err
	}
	response.ExpiresIn = max(0, int64(repository.metadata.AccessExpiresAt.Sub(s.now().UTC()).Seconds()))
	return RuntimeTokenResponse{RuntimeManagementTokens: runtimeManagementTokens(response)}, nil
}

// RuntimeManagementTokens uses the standard OAuth wire fields and stays separate from execution credentials.
type RuntimeManagementTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// runtimeManagementTokens adapts the shared protocol response without exposing its internal Go field names.
func runtimeManagementTokens(tokens TokenResponse) *RuntimeManagementTokens {
	return &RuntimeManagementTokens{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, TokenType: tokens.TokenType, ExpiresIn: tokens.ExpiresIn, Scope: strings.Join(tokens.Scope, " ")}
}

// MarshalJSON keeps internal contract ceiling proofs in storage rather than duplicating them in bounded controller responses.
func (response RuntimeTokenResponse) MarshalJSON() ([]byte, error) {
	type wire RuntimeTokenResponse
	response.OAuthRuntimeGrant.ContractBindings = nil
	return json.Marshal(wire(response))
}
