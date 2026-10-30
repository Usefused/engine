package oauthprovider

import (
	"context"
	"slices"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/fused-open-core/oauthserver"
	"github.com/google/uuid"
)

// MCPRegistration is the RFC 7591 subset needed by browser-based MCP clients.
type MCPRegistration struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
}

// RegisterMCPClient creates a bounded provisional identity; only browser consent can make it durable.
func (s *Service) RegisterMCPClient(ctx context.Context, req MCPRegistration) (CreateClientResult, error) {
	// Registration cannot request management permissions or unsupported grant types.
	if err := validateMCPRegistration(req); err != nil {
		return CreateClientResult{}, err
	}
	name := strings.TrimSpace(req.ClientName)
	// Anonymous clients still need a readable label on the consent screen.
	if name == "" {
		name = "MCP client"
	}
	kind := store.OAuthClientConfidential
	// Only explicit public registration omits client-secret authentication.
	if req.TokenEndpointAuthMethod == "none" {
		kind = store.OAuthClientPublic
	}
	return s.registerClient(ctx, store.OAuthClientRegistration{Name: name, ClientType: kind, RedirectURIs: req.RedirectURIs, AllowedScopes: []string{store.OAuthMCPScope}})
}

// validateMCPRegistration admits only authorization-code clients with optional refresh support.
func validateMCPRegistration(req MCPRegistration) error {
	// One runtime scope is the entire registration ceiling, regardless of client metadata.
	if req.Scope != "" && req.Scope != store.OAuthMCPScope {
		return ErrInvalidScope
	}
	// Unsupported authentication methods must not silently become public clients.
	if !slices.Contains([]string{"", "none", "client_secret_basic", "client_secret_post"}, req.TokenEndpointAuthMethod) {
		return ErrInvalidRequest
	}
	// Client credentials and implicit grants cannot represent browser-approved MCP access.
	for _, grant := range req.GrantTypes {
		// Machine-to-machine and implicit grants cannot replace browser consent.
		if grant != "authorization_code" && grant != "refresh_token" {
			return ErrUnsupportedGrantType
		}
	}
	// The issuer supports codes only, never front-channel access tokens.
	for _, response := range req.ResponseTypes {
		// Tokens must be exchanged through the back channel after PKCE verification.
		if response != "code" {
			return ErrInvalidRequest
		}
	}
	return nil
}

// mcpStore shares OAuth persistence but requires explicit action consent on every new connection.
type mcpStore struct{ *requestStore }

// GetOAuthUserConsent must not treat another server's coarse scope as consent for this server.
func (s mcpStore) GetOAuthUserConsent(context.Context, uuid.UUID, uuid.UUID) (store.OAuthConsent, bool, error) {
	return store.OAuthConsent{}, false, nil
}

// GetOAuthClientByPublicID prevents management clients from entering the runtime issuer.
func (s mcpStore) GetOAuthClientByPublicID(ctx context.Context, id string) (oauthserver.OAuthClient, string, error) {
	client, hash, err := s.requestStore.GetOAuthClientByPublicID(ctx, id)
	// Mixed or management scopes cannot be converted into runtime credentials.
	if err != nil || !store.OAuthMCPOnly(client.AllowedScopes) {
		return oauthserver.OAuthClient{}, "", store.ErrOAuthClientNotFound
	}
	return client, hash, nil
}

// mcpCore reuses the protocol implementation with a request-local runtime-only client boundary.
func (s *Service) mcpCore() (*oauthserver.Service[uuid.UUID], *requestStore, error) {
	repository := &requestStore{coreStore: coreStore{s.store}}
	core, err := newCore(mcpStore{repository}, s.revisions)
	return core, repository, err
}

// mcpActor permits the protocol scope only after the HTTP adapter has checked the exact server's rights.
func mcpActor(ctx context.Context, actor accesscontrol.Actor) oauthserver.Actor[uuid.UUID] {
	result := adaptActor(ctx, actor)
	// No control permission can be requested through this authorization flow.
	result.CanGrant = func(_ context.Context, scope string) (bool, error) { return scope == store.OAuthMCPScope, nil }
	return result
}

// AuthorizeMCP validates client, redirect, PKCE, and scope before displaying action consent.
func (s *Service) AuthorizeMCP(ctx context.Context, actor accesscontrol.Actor, req AuthorizeRequest) (AuthorizeResult, error) {
	core, repository, err := s.mcpCore()
	// A missing protocol adapter must not render apparently valid consent.
	if err != nil {
		return AuthorizeResult{}, err
	}
	result, err := core.Authorize(ctx, mcpActor(ctx, actor), req)
	return AuthorizeResult{RequiresConsent: result.RequiresConsent, Client: repository.client, Scope: result.Scope}, authenticationError(err)
}

// ConsentMCP stores the selected runtime policy through the same single-use code transaction.
func (s *Service) ConsentMCP(ctx context.Context, actor accesscontrol.Actor, req ConsentRequest, grant store.OAuthMCPGrant) (string, error) {
	core, _, err := s.mcpCore()
	// No grant is issued if the shared protocol service cannot be constructed.
	if err != nil {
		return "", err
	}
	return core.Consent(store.ContextWithOAuthMCP(ctx, grant.Resource, &grant), mcpActor(ctx, actor), req)
}

// TokenMCP preserves the original audience and action policy through exchange and rotation.
func (s *Service) TokenMCP(ctx context.Context, req TokenRequest, resource string) (TokenResponse, error) {
	core, repository, err := s.mcpCore()
	// Empty audiences cannot fall back to a management-token exchange.
	if err != nil || resource == "" {
		return TokenResponse{}, ErrInvalidRequest
	}
	response, err := core.Token(store.ContextWithOAuthMCP(ctx, resource, nil), req)
	// Return only the actual persisted lifetime, including client expiry caps.
	if err != nil {
		return TokenResponse{}, err
	}
	response.ExpiresIn = max(0, int64(repository.metadata.AccessExpiresAt.Sub(s.now().UTC()).Seconds()))
	return response, nil
}
