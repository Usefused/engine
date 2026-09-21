package managedauthclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/managedpublication"

	"github.com/Usefused/engine/internal/engine/managedauthtransport"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// ErrManagedAppNotRegistered means Fused has not registered a managed app
// for this (service, auth scheme) -- the caller should fall back to
// requiring the workspace's own application credentials, exactly as if
// managed auth were never enabled.
var ErrManagedAppNotRegistered = fmt.Errorf("no Fused Managed App is registered for this service")

// ConnectClient calls the broker's connect-time proxy
// (managedauthbroker.MountConnectRoutes) so the connect flow can use a
// Fused Managed App instead of a workspace-owned OAuth application. It is
// the client-side counterpart of managedauthbroker.ConnectService.
type ConnectClient struct {
	brokerURL  string
	tokens     *Service
	httpClient *http.Client
}

// NewConnectClient confines credentials to the configured broker without mutating caller transport settings.
func NewConnectClient(brokerURL string, tokens *Service, httpClient *http.Client) *ConnectClient {
	httpClient = managedauthtransport.New(httpClient, 20*time.Second)
	return &ConnectClient{brokerURL: strings.TrimRight(brokerURL, "/"), tokens: tokens, httpClient: httpClient}
}

// ClientID returns the public client_id of Fused's registered app for
// serviceID/authName, or ErrManagedAppNotRegistered if there is none.
func (c *ConnectClient) ClientID(ctx context.Context, serviceID uuid.UUID, authName string, applicationIDs ...string) (string, error) {
	// A disabled optional remote adapter must fail closed even when carried in a typed interface.
	if c == nil || c.tokens == nil {
		return "", ErrNotEnrolled
	}
	accessToken, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.path(serviceID, authName, "client-id", applicationIDs...), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	var result struct {
		ClientID string `json:"client_id"`
	}
	if err := c.do(request, &result); err != nil {
		return "", err
	}
	return result.ClientID, nil
}

// Exchange redeems a provider authorization code through the broker, the
// managed-auth counterpart of connectauth.ExchangeAuthorizationCode.
func (c *ConnectClient) Exchange(ctx context.Context, serviceID uuid.UUID, authName, redirectURI string, auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract, code, verifier string, applicationIDs ...string) (connectauth.TokenResponse, error) {
	return c.grant(ctx, serviceID, authName, "exchange", map[string]any{
		"redirect_uri": redirectURI, "auth": auth, "flow": flow, "code": code, "verifier": verifier,
	}, applicationIDs...)
}

// Refresh is the managed-auth counterpart of connectauth.RefreshAccessToken.
func (c *ConnectClient) Refresh(ctx context.Context, serviceID uuid.UUID, authName, redirectURI string, auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract, refreshToken string, applicationIDs ...string) (connectauth.TokenResponse, error) {
	return c.grant(ctx, serviceID, authName, "refresh", map[string]any{
		"redirect_uri": redirectURI, "auth": auth, "flow": flow, "refresh_token": refreshToken,
	}, applicationIDs...)
}

// grant carries the same explicit publication selector for code exchange and refresh.
func (c *ConnectClient) grant(ctx context.Context, serviceID uuid.UUID, authName, action string, body map[string]any, applicationIDs ...string) (connectauth.TokenResponse, error) {
	accessToken, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return connectauth.TokenResponse{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return connectauth.TokenResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path(serviceID, authName, action, applicationIDs...), bytes.NewReader(payload))
	if err != nil {
		return connectauth.TokenResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	var token connectauth.TokenResponse
	if err := c.do(request, &token); err != nil {
		return connectauth.TokenResponse{}, err
	}
	return token, nil
}

// path keeps provider scheme and application identity separate and escapes both on the wire.
func (c *ConnectClient) path(serviceID uuid.UUID, authName, action string, applicationIDs ...string) string {
	id, err := managedpublication.Selector(applicationIDs)
	// Malformed selectors are sent as an invalid value, never silently mapped onto the default.
	if err != nil {
		id = "invalid"
	}
	base := c.brokerURL + "/managed-auth/broker/connect/" + serviceID.String() + "/" + url.PathEscape(authName)
	// Named publications get an explicit path; the empty selector preserves the legacy wire route.
	if id != "" {
		base += "/applications/" + url.PathEscape(id)
	}
	return base + "/" + action
}

func (c *ConnectClient) do(request *http.Request, out any) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("managed-auth connect request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrManagedAppNotRegistered
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("managed-auth connect request rejected (status %d)", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(out); err != nil {
		return fmt.Errorf("decode managed-auth connect response: %w", err)
	}
	return nil
}
