package connectauth

import (
	"context"
	"net/http"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// DelegatedOAuth admits only OAuth operations on an explicitly published remote registration.
type DelegatedOAuth interface {
	ClientID(context.Context, uuid.UUID, string) (string, error)
	Exchange(context.Context, uuid.UUID, string, string, fusedobject.AuthConfig, fusedobject.OAuth2FlowContract, string, string) (TokenResponse, error)
	Refresh(context.Context, uuid.UUID, string, string, fusedobject.AuthConfig, fusedobject.OAuth2FlowContract, string) (TokenResponse, error)
}

// OAuthGrant is the shared connect/refresh boundary after source selection; it owns neither user-token storage nor scheduling.
type OAuthGrant interface {
	Exchange(context.Context, string, string) (TokenResponse, error)
	Refresh(context.Context, string) (TokenResponse, error)
}

// ApplicationRequest carries the exact existing connection identity and local contract into source resolution.
type ApplicationRequest struct {
	BucketID, ServiceID uuid.UUID
	AuthType, AuthName  string
	Source              ApplicationCredentialSource
	Auth                fusedobject.AuthConfig
	Flow                fusedobject.OAuth2FlowContract
}

// ResolveApplication chooses local or delegated operations once; a missing local credential never selects a remote source.
func (r *ApplicationCredentialResolver) ResolveApplication(ctx context.Context, request ApplicationRequest, client *http.Client, remote DelegatedOAuth) (ClientCredentials, OAuthGrant, error) {
	// Only an explicit source reference may select a delegated registration.
	if request.Source.Managed {
		return r.resolveDelegated(ctx, request, remote)
	}
	creds, err := r.Resolve(ctx, request.BucketID, request.ServiceID, request.AuthType, request.AuthName, request.Source)
	// Resolve errors preserve the ordinary local missing-registration behavior.
	if err != nil {
		return ClientCredentials{}, nil, err
	}
	return creds, localOAuthGrant{client: client, auth: request.Auth, flow: request.Flow, creds: creds}, nil
}

// resolveDelegated binds the exact source identity so later callback and refresh code cannot accidentally use a target service.
func (r *ApplicationCredentialResolver) resolveDelegated(ctx context.Context, request ApplicationRequest, remote DelegatedOAuth) (ClientCredentials, OAuthGrant, error) {
	source := request.Source
	// Missing authority or a partial reference cannot become a remotely selected default registration.
	if remote == nil || source.ServiceID == uuid.Nil || source.AuthName == "" {
		return ClientCredentials{}, nil, ErrApplicationCredentialsUnavailable
	}
	id, err := remote.ClientID(ctx, source.ServiceID, source.AuthName)
	// Discovery never returns a client secret and must succeed before constructing a grant adapter.
	if err != nil || id == "" {
		return ClientCredentials{}, nil, ErrApplicationCredentialsUnavailable
	}
	creds := ClientCredentials{ClientID: id, RedirectURI: r.redirectURI}
	return creds, delegatedOAuthGrant{remote: remote, source: source, auth: request.Auth, flow: request.Flow, redirectURI: r.redirectURI}, nil
}

type localOAuthGrant struct {
	client *http.Client
	auth   fusedobject.AuthConfig
	flow   fusedobject.OAuth2FlowContract
	creds  ClientCredentials
}

// Exchange preserves the existing core OAuth behavior for customer-owned registrations.
func (g localOAuthGrant) Exchange(ctx context.Context, code, verifier string) (TokenResponse, error) {
	return ExchangeAuthorizationCode(ctx, g.client, g.auth, g.flow, g.creds, code, verifier)
}

// Refresh shares the ordinary token helper while the caller retains its existing connection lease.
func (g localOAuthGrant) Refresh(ctx context.Context, token string) (TokenResponse, error) {
	return RefreshAccessToken(ctx, g.client, g.auth, g.flow, g.creds, token)
}

type delegatedOAuthGrant struct {
	remote      DelegatedOAuth
	source      ApplicationCredentialSource
	auth        fusedobject.AuthConfig
	flow        fusedobject.OAuth2FlowContract
	redirectURI string
}

// Exchange delegates only the grant operation; callback ownership and token persistence stay with the customer Engine.
func (g delegatedOAuthGrant) Exchange(ctx context.Context, code, verifier string) (TokenResponse, error) {
	return g.remote.Exchange(ctx, g.source.ServiceID, g.source.AuthName, g.redirectURI, g.auth, g.flow, code, verifier)
}

// Refresh sends the current user token to its exact published source, never a generic credential proxy.
func (g delegatedOAuthGrant) Refresh(ctx context.Context, token string) (TokenResponse, error) {
	return g.remote.Refresh(ctx, g.source.ServiceID, g.source.AuthName, g.redirectURI, g.auth, g.flow, token)
}
