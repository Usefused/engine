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
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.oauth.client.register")
	defer span.End()
	// A local callback keeps this self-service flow on the user's machine.
	if err := validateLoopbackRedirectURI(req.RedirectURI); err != nil {
		return RegisterClientResult{}, err
	}
	clientID, err := randomIdentifier(clientIDPrefix, 16)
	// Entropy failure must never produce a predictable client identity.
	if err != nil {
		return RegisterClientResult{}, err
	}
	rawSecret, err := randomIdentifier(clientSecretPrefix, 32)
	// A confidential client must always have independently generated proof.
	if err != nil {
		return RegisterClientResult{}, err
	}
	name := strings.TrimSpace(req.Name)
	// Give consent a readable label when the client omits its display name.
	if name == "" {
		name = "Dynamic client"
	}
	expiresAt := s.now().Add(ephemeralClientTTL)
	// Persist only the hash and never attribute anonymous registration to a user.
	if _, err := s.store.RegisterOAuthClient(ctx, store.OAuthClientRegistration{
		Name: name, ClientType: store.OAuthClientConfidential, ClientID: clientID,
		ClientSecretHash: hashSecret(rawSecret), RedirectURIs: []string{req.RedirectURI},
		AllowedScopes: req.Scopes, ExpiresAt: &expiresAt,
		Actor: store.MutationActor{RequestID: middleware.GetReqID(ctx), TraceID: traceID(ctx)},
	}); err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		// Invalid metadata is a caller error, not an Engine failure.
		if errors.Is(err, store.ErrInvalidOAuthClient) {
			return RegisterClientResult{}, fmt.Errorf("%w: invalid client metadata", ErrInvalidRequest)
		}
		return RegisterClientResult{}, err
	}
	span.SetAttributes(attribute.String("outcome", "created"), attribute.String("oauth.client_id", clientID))
	return RegisterClientResult{ClientID: clientID, ClientSecret: rawSecret, ClientIDExpiresAt: expiresAt}, nil
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
