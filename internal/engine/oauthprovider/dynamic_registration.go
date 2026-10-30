package oauthprovider

// Temporary registration is Fused policy absent from the pinned shared core.
// It creates no grant; authorization and token issuance still run through the core.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const ephemeralClientTTL = store.OAuthDynamicClientMaxTTL

// RegisterClientRequest asks for temporary credentials without identifying a
// workspace user; only browser login and consent can authorize Engine access.
type RegisterClientRequest struct {
	RedirectURI string
	Scopes      []string
	Name        string
}

// RegisterClientResult returns the secret once, alongside the shared expiry.
type RegisterClientResult struct {
	ClientID          string
	ClientSecret      string
	ClientIDExpiresAt time.Time
}

// RegisterClient mints a short-lived client pair bound to a loopback callback.
// Registration grants no user permissions; consent and PKCE remain mandatory.
func (s *Service) RegisterClient(ctx context.Context, req RegisterClientRequest) (RegisterClientResult, error) {
	// CLI registration must remain loopback-only even though MCP supports remote callbacks.
	if err := validateLoopbackRedirectURI(req.RedirectURI); err != nil {
		return RegisterClientResult{}, err
	}
	name := strings.TrimSpace(req.Name)
	// Give local clients a readable consent label when they omit one.
	if name == "" {
		name = "Dynamic client"
	}
	result, err := s.registerClient(ctx, store.OAuthClientRegistration{Name: name, ClientType: store.OAuthClientConfidential, RedirectURIs: []string{req.RedirectURI}, AllowedScopes: req.Scopes})
	// Never expose a partial credential pair after failed registration.
	if err != nil {
		return RegisterClientResult{}, err
	}
	return RegisterClientResult{ClientID: result.Client.ClientID, ClientSecret: result.ClientSecret, ClientIDExpiresAt: *result.Client.ExpiresAt}, nil
}

// registerClient owns anonymous client identity, secret hashing, expiry, and audit attribution for both registration formats.
func (s *Service) registerClient(ctx context.Context, input store.OAuthClientRegistration) (CreateClientResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.client.register")
	defer span.End()
	clientID, err := randomIdentifier(clientIDPrefix, 16)
	// Entropy failure must never produce a predictable client identity.
	if err != nil {
		return CreateClientResult{}, err
	}
	var rawSecret string
	// Public clients use PKCE only; confidential clients also need independent client authentication.
	if input.ClientType == store.OAuthClientConfidential {
		rawSecret, err = randomIdentifier(clientSecretPrefix, 32)
		// Secret-generation failure cannot silently downgrade a confidential client.
		if err != nil {
			return CreateClientResult{}, err
		}
		input.ClientSecretHash = hashSecret(rawSecret)
	}
	expiresAt := s.now().Add(ephemeralClientTTL)
	input.ClientID, input.ExpiresAt = clientID, &expiresAt
	input.Actor = store.MutationActor{RequestID: middleware.GetReqID(ctx), TraceID: traceID(ctx)}
	result, err := s.store.RegisterOAuthClient(ctx, input)
	// No registration is successful until persistence and its audit commit together.
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		// Invalid metadata is a caller error, not an Engine failure.
		if errors.Is(err, store.ErrInvalidOAuthClient) {
			return CreateClientResult{}, fmt.Errorf("%w: invalid client metadata", ErrInvalidRequest)
		}
		return CreateClientResult{}, err
	}
	span.SetAttributes(attribute.String("outcome", "created"), attribute.String("oauth.client_id", clientID))
	return CreateClientResult{Client: result.Client, ClientSecret: rawSecret}, nil
}

// validateLoopbackRedirectURI admits only explicit HTTP loopback redirects for
// dynamically-registered clients: the authorization code lands on the authorizing
// user's own machine, so a remote attacker cannot harvest it.
func validateLoopbackRedirectURI(raw string) error {
	parsed, err := url.Parse(raw)
	// Reject ambiguous callbacks before accepting a local client.
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%w: redirect_uri must be an HTTP loopback URL", ErrInvalidRequest)
	}
	host := parsed.Hostname()
	// Only literal loopback hosts may receive anonymous-client codes.
	if !strings.EqualFold(host, "localhost") && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("%w: redirect_uri must be a loopback address", ErrInvalidRequest)
	}
	return nil
}

// randomIdentifier supplies unpredictable, namespaced credentials for temporary clients.
func randomIdentifier(prefix string, byteLength int) (string, error) {
	buf := make([]byte, byteLength)
	// Entropy failure must never produce a usable client credential.
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashSecret keeps raw credentials out of persistent storage.
func hashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
