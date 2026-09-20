package managedauthclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/managedauthtransport"
)

// ErrBrokerRejected means the broker itself refused the call (invalid
// ticket, expired/revoked refresh token) -- distinct from a transport
// failure, so callers can tell "try again" apart from "re-enroll."
type ErrBrokerRejected struct{ StatusCode int }

func (e ErrBrokerRejected) Error() string {
	return fmt.Sprintf("managed-auth broker rejected the request (status %d)", e.StatusCode)
}

// HTTPBrokerClient calls the managed-auth broker's own enrollment/refresh
// endpoints (managedauthbroker.MountRoutes on the broker side).
type HTTPBrokerClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewHTTPBrokerClient confines credentials to the configured broker without mutating caller transport settings.
func NewHTTPBrokerClient(brokerURL string, httpClient *http.Client) *HTTPBrokerClient {
	httpClient = managedauthtransport.New(httpClient, 15*time.Second)
	return &HTTPBrokerClient{baseURL: strings.TrimRight(brokerURL, "/"), httpClient: httpClient}
}

type brokerTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (c *HTTPBrokerClient) Enroll(ctx context.Context, ticket string) (accessToken, refreshToken string, expiresIn int64, err error) {
	return c.call(ctx, "/managed-auth/broker/enroll", map[string]string{"ticket": ticket})
}

// Refresh presents a fresh Registry ticket alongside the current rotation credential.
func (c *HTTPBrokerClient) Refresh(ctx context.Context, refreshToken, ticket string) (newAccessToken, newRefreshToken string, expiresIn int64, err error) {
	return c.call(ctx, "/managed-auth/broker/refresh", map[string]string{"refresh_token": refreshToken, "ticket": ticket})
}

func (c *HTTPBrokerClient) call(ctx context.Context, path string, body map[string]string) (string, string, int64, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", "", 0, fmt.Errorf("encode managed-auth broker request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return "", "", 0, fmt.Errorf("build managed-auth broker request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", "", 0, fmt.Errorf("managed-auth broker request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", 0, ErrBrokerRejected{StatusCode: response.StatusCode}
	}
	var token brokerTokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&token); err != nil {
		return "", "", 0, fmt.Errorf("decode managed-auth broker response: %w", err)
	}
	return token.AccessToken, token.RefreshToken, token.ExpiresIn, nil
}

// Revoke is idempotent and needs only the installation's own refresh credential, even after license withdrawal.
func (c *HTTPBrokerClient) Revoke(ctx context.Context, refreshToken string) error {
	_, _, _, err := c.call(ctx, "/managed-auth/broker/revoke", map[string]string{"refresh_token": refreshToken})
	return err
}
