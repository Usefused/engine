package managedauthbroker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

var errInvalidConnectServiceConfig = errors.New("invalid managed-auth connect service configuration")

// ConnectService performs the third-party OAuth handshake on behalf of an
// enrolled installation using Fused's own registered application, so the
// application's client secret is sent only to its operator-approved token endpoint.
// It calls exactly
// the same connectauth exchange/refresh functions a customer Engine would
// call locally for its own OAuth app; only where the client credentials
// and token policy come from differs.
type ConnectService struct {
	catalog    ProviderAppReader
	masterKey  []byte
	httpClient *http.Client
	Proof      *webhookrelay.ProofStore
}

// NewConnectService isolates credential-bearing HTTP requests from caller-owned redirect policy.
func NewConnectService(catalog ProviderAppReader, masterKey []byte, httpClient *http.Client) (*ConnectService, error) {
	// Incomplete dependencies must never permit credential lookup.
	if catalog == nil || len(masterKey) != 32 {
		return nil, errInvalidConnectServiceConfig
	}
	// Preserve the supplied transport (including test TLS roots) without modifying a shared client.
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	boundedClient := *httpClient
	// OAuth token endpoints must not forward secrets through any redirect, including same-origin 307/308 responses.
	boundedClient.CheckRedirect = rejectTokenRedirect
	// Every provider request must have a finite upper bound even when the default client is supplied.
	if boundedClient.Timeout <= 0 || boundedClient.Timeout > 25*time.Second {
		boundedClient.Timeout = 25 * time.Second
	}
	return &ConnectService{catalog: catalog, masterKey: masterKey, httpClient: &boundedClient}, nil
}

// ClientID returns the public client_id an installation needs to build its
// own authorization redirect; the secret is never exposed.
func (s *ConnectService) ClientID(ctx context.Context, serviceID uuid.UUID, authName string) (string, error) {
	app, err := s.providerApp(ctx, serviceID, authName)
	// Invalid or unavailable operator policy must fail before a credential-bearing network request.
	if err != nil {
		return "", err
	}
	return app.ClientID, nil
}

// Exchange redeems a provider authorization code using Fused's registered
// application, mirroring connectauth.ExchangeAuthorizationCode exactly
// except that credentials and token policy come exclusively from the operator catalogue.
// Legacy auth/flow arguments are ignored so older consumers cannot select a credential destination.
func (s *ConnectService) Exchange(ctx context.Context, serviceID uuid.UUID, authName, redirectURI string, _ fusedobject.AuthConfig, _ fusedobject.OAuth2FlowContract, code, verifier string) (connectauth.TokenResponse, error) {
	app, err := s.providerApp(ctx, serviceID, authName)
	// Invalid or unavailable operator policy must fail before a credential-bearing network request.
	if err != nil {
		return connectauth.TokenResponse{}, err
	}
	auth, flow := app.Auth, app.Flow
	creds := connectauth.ClientCredentials{ClientID: app.ClientID, ClientSecret: app.ClientSecret, RedirectURI: redirectURI}
	token, err := connectauth.ExchangeAuthorizationCode(ctx, s.httpClient, auth, flow, creds, code, verifier)
	// Only a successful provider response can establish a remote webhook grant.
	if err == nil && s.Proof != nil {
		installation, ok := ctx.Value(installationContextKey{}).(Installation)
		// Proof creation requires the authenticated installation resolved by the broker middleware.
		if !ok {
			return connectauth.TokenResponse{}, webhookrelay.ErrDenied
		}
		err = s.Proof.RecordExchange(ctx, installation.ID, serviceID, authName, token.AccessToken, token.RefreshToken, token.RawResponse)
	}
	token.RawResponse = nil
	return token, err
}

// Refresh mirrors connectauth.RefreshAccessToken the same way Exchange
// mirrors ExchangeAuthorizationCode.
func (s *ConnectService) Refresh(ctx context.Context, serviceID uuid.UUID, authName, redirectURI string, _ fusedobject.AuthConfig, _ fusedobject.OAuth2FlowContract, refreshToken string) (connectauth.TokenResponse, error) {
	app, err := s.providerApp(ctx, serviceID, authName)
	// Invalid or unavailable operator policy must fail before a credential-bearing network request.
	if err != nil {
		return connectauth.TokenResponse{}, err
	}
	auth, flow := app.Auth, app.Flow
	creds := connectauth.ClientCredentials{ClientID: app.ClientID, ClientSecret: app.ClientSecret, RedirectURI: redirectURI}
	token, err := connectauth.RefreshAccessToken(ctx, s.httpClient, auth, flow, creds, refreshToken)
	// Refresh moves only the existing installation's proof; it cannot introduce a new provider resource.
	if err == nil && s.Proof != nil {
		installation, ok := ctx.Value(installationContextKey{}).(Installation)
		// An unauthenticated internal caller cannot rotate another receiver's proof.
		if !ok {
			return connectauth.TokenResponse{}, webhookrelay.ErrDenied
		}
		err = s.Proof.RecordRefresh(ctx, installation.ID, serviceID, authName, refreshToken, token.AccessToken, token.RefreshToken)
	}
	token.RawResponse = nil
	return token, err
}

// ProviderAppReader separates trusted registration lookup from caller-supplied OAuth metadata.
type ProviderAppReader interface {
	GetProviderApp(context.Context, uuid.UUID, string, []byte) (ProviderApp, error)
}

// providerApp validates at the use boundary so legacy rows or alternate readers cannot bypass policy validation.
func (s *ConnectService) providerApp(ctx context.Context, serviceID uuid.UUID, authName string) (ProviderApp, error) {
	app, err := s.catalog.GetProviderApp(ctx, serviceID, authName, s.masterKey)
	// Never construct a provider request from a failed lookup.
	if err != nil {
		return ProviderApp{}, err
	}
	// A stored client pair alone is insufficient authority to send its secret anywhere.
	if err := validatePublishedContract(app.Auth, app.Flow); err != nil {
		return ProviderApp{}, err
	}
	return app, nil
}

// rejectTokenRedirect prevents HTTP redirect responses from moving credentials beyond the pinned endpoint.
func rejectTokenRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
