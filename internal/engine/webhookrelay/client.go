package webhookrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AccessTokens reuses the installation's existing enrolled credential and revocation behavior.
type AccessTokens interface {
	AccessToken(context.Context) (string, error)
}

// Client uses only the Registry-advertised broker, with no redirects or customer-supplied receiver URLs.
type Client struct {
	URL    string
	Tokens AccessTokens
	HTTP   *http.Client
}

// NewClient restricts credential transport to HTTPS, allowing loopback HTTP solely for local Engine tests.
func NewClient(raw string, tokens AccessTokens, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(raw)
	// User information, query strings and fragments cannot be part of a credential-bearing origin.
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrDenied
	}
	// Cleartext transport is restricted to literal local addresses, avoiding DNS-dependent exceptions.
	if u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()) {
		return nil, ErrDenied
	}
	// The ordinary production transport uses standard TLS certificate verification.
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	bounded := *httpClient
	bounded.Timeout = 10 * time.Second
	bounded.CheckRedirect = rejectRedirect
	return &Client{URL: strings.TrimRight(raw, "/"), Tokens: tokens, HTTP: &bounded}, nil
}

// rejectRedirect keeps provider and installation credentials at the configured broker origin.
func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

// call authenticates each operation afresh so disabling managed auth immediately stops outbound delivery work.
func (c *Client) call(ctx context.Context, method, path string, input, output any) error {
	token, err := c.Tokens.AccessToken(ctx)
	// Disabled, expired or unavailable installation credentials cannot authorize a request.
	if err != nil {
		return err
	}
	body, err := json.Marshal(input)
	// Input must be completely encoded before a credential-bearing request is created.
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+"/managed-auth/broker/webhooks/"+path, bytes.NewReader(body))
	// Invalid configured destinations fail locally without attempting alternate URLs.
	if err != nil {
		return ErrDenied
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	// Transport errors are deliberately bounded so URLs or credentials cannot leak through worker logs.
	if err != nil {
		return fmt.Errorf("webhook broker unavailable")
	}
	defer resp.Body.Close()
	// Empty polls and idempotent acknowledgements use the same no-content success status.
	if resp.StatusCode == 204 {
		return nil
	}
	// Raw broker bodies are never included in diagnostic errors.
	if resp.StatusCode != 200 {
		return ErrDenied
	}
	// Mutations with no return shape still require an HTTP success from the broker.
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 3<<20)).Decode(output)
}

// Subscribe presents provider-token possession only to the broker which originally exchanged that token.
func (c *Client) Subscribe(ctx context.Context, registration, receiver uuid.UUID, token string) (uuid.UUID, error) {
	var response struct {
		ID uuid.UUID `json:"subscription_id"`
	}
	err := c.call(ctx, "POST", "subscriptions", map[string]any{"registration_id": registration, "receiver_id": receiver, "access_token": token}, &response)
	return response.ID, err
}

// Pull receives a single audience-bound event; nil denotes a successful idle poll.
func (c *Client) Pull(ctx context.Context, id uuid.UUID) (*Delivery, error) {
	var d *Delivery
	err := c.call(ctx, "POST", "subscriptions/"+id.String()+"/pull", nil, &d)
	return d, err
}

// Ack runs only after the local stream acknowledged durable publication.
func (c *Client) Ack(ctx context.Context, id uuid.UUID, receipt string) error {
	return c.call(ctx, "POST", "subscriptions/"+id.String()+"/ack", map[string]string{"receipt": receipt}, nil)
}

// Revoke durably withdraws only this installation's explicit subscription.
func (c *Client) Revoke(ctx context.Context, id uuid.UUID) error {
	return c.call(ctx, "DELETE", "subscriptions/"+id.String(), nil, nil)
}

// RevokeReceiver settles ambiguous setup responses using the durable consumer-selected receiver identity.
func (c *Client) RevokeReceiver(ctx context.Context, id uuid.UUID) error {
	return c.call(ctx, "DELETE", "receivers/"+id.String(), nil, nil)
}
