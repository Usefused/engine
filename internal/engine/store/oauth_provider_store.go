package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

type OAuthClientType string

const (
	OAuthClientConfidential OAuthClientType = "confidential"
	OAuthClientPublic       OAuthClientType = "public"

	maxOAuthClientNameLength  = 200
	maxOAuthRedirectURIs      = 10
	maxOAuthScopeCount        = 32
	oauthAuthorizationCodeTTL = 60 * time.Second
)

var (
	ErrInvalidOAuthClient      = errors.New("invalid OAuth client")
	ErrOAuthClientNotFound     = errors.New("OAuth client not found")
	ErrOAuthGrantDenied        = errors.New("OAuth grant denied")
	ErrOAuthGrantReuseDetected = errors.New("OAuth refresh token reuse detected")
	ErrOAuthConsentNotFound    = errors.New("OAuth consent not found")
	ErrOAuthRegistrationKeyNotFound = errors.New("OAuth registration key not found")
)

// OAuthClient is the sanitized, secret-free projection of a registered
// third-party client returned to admin UI/API callers.
type OAuthClient struct {
	ID            uuid.UUID
	ClientID      string
	Name          string
	ClientType    OAuthClientType
	RedirectURIs  []string
	AllowedScopes []string
	HasSecret     bool
	CreatedAt     time.Time
	// ExpiresAt marks an ephemeral client minted by POST /oauth/register; it is
	// nil for the long-lived clients created through the admin surface.
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// OAuthClientRegistration carries store-ready crypto material: the caller
// (oauthprovider.Service) generates the public client_id and, for confidential
// clients, hashes a freshly minted secret before this ever reaches the store
// -- the store never sees or returns a raw secret, matching every other
// credential in Engine.
type OAuthClientRegistration struct {
	Name             string
	ClientType       OAuthClientType
	ClientID         string
	ClientSecretHash string
	RedirectURIs     []string
	AllowedScopes    []string
	// ExpiresAt is set only for ephemeral dynamically-registered clients.
	ExpiresAt *time.Time
	Actor     MutationActor
}

type OAuthConsent struct {
	ClientID     uuid.UUID
	SubjectID    uuid.UUID
	GrantedScope []string
	GrantedAt    time.Time
}

type OAuthConsentGrant struct {
	ClientID     uuid.UUID
	SubjectID    uuid.UUID
	GrantedScope []string
}

type OAuthAuthorizationCodeIssue struct {
	ID            uuid.UUID
	ClientID      uuid.UUID
	SubjectID     uuid.UUID
	RedirectURI   string
	Scope         []string
	CodeHash      string
	CodeChallenge string
	ExpiresAt     time.Time
}

// OAuthCodeExchange is the inbound authorization_code grant request. CodeHash
// identifies the code; ClientID/RedirectURI/CodeVerifier are all re-checked
// against the values recorded at Consent time before any token is minted.
type OAuthCodeExchange struct {
	CodeHash     string
	ClientID     uuid.UUID
	RedirectURI  string
	CodeVerifier string
	Issue        OAuthTokenIssue
}

// OAuthTokenIssue carries only the freshly generated credential material for
// one token pair; the store assigns its row id and, for a brand-new grant, a
// new token_family_id -- rotation instead inherits the family id of the
// refresh token being consumed, which only the store has locked and can see.
type OAuthTokenIssue struct {
	AccessTokenHash  string
	RefreshTokenHash string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

type OAuthTokenMetadata struct {
	ID                    uuid.UUID
	ClientID              uuid.UUID
	SubjectID             uuid.UUID
	TokenFamilyID         uuid.UUID
	Scope                 []string
	AccessExpiresAt       time.Time
	RefreshExpiresAt      time.Time
	AuthorizationRevision int64
}

type OAuthClientMutationResult struct {
	Client                OAuthClient
	AuthorizationRevision int64
}

// OAuthRefreshExchange is the inbound refresh_token grant request.
type OAuthRefreshExchange struct {
	RefreshTokenHash string
	ClientID         uuid.UUID
	Issue            OAuthTokenIssue
}

type OAuthConnectedApp struct {
	ClientID     uuid.UUID
	ClientName   string
	GrantedScope []string
	GrantedAt    time.Time
}

// OAuthRegistrationKey is the secret-free projection of a per-user registration
// key, surfaced in settings for mint/rotate/revoke. The raw value is only ever
// returned at mint time by the service, never persisted.
type OAuthRegistrationKey struct {
	ID        uuid.UUID
	SubjectID uuid.UUID
	CreatedAt time.Time
	RevokedAt *time.Time
}

type OAuthClientStore interface {
	CreateOAuthClient(context.Context, OAuthClientRegistration) (OAuthClientMutationResult, error)
	// RegisterOAuthClient persists a dynamically-registered ephemeral client on
	// behalf of a registration-key subject; it requires no active control
	// credential because the caller already authenticated the key.
	RegisterOAuthClient(context.Context, OAuthClientRegistration) (OAuthClientMutationResult, error)
	ListOAuthClients(context.Context) ([]OAuthClient, error)
	RevokeOAuthClient(context.Context, uuid.UUID, MutationActor) (int64, error)

	GetOAuthClientByPublicID(context.Context, string) (OAuthClient, string, error)
	GetOAuthUserConsent(context.Context, uuid.UUID, uuid.UUID) (OAuthConsent, bool, error)
	RecordOAuthConsentAndIssueCode(context.Context, OAuthConsentGrant, OAuthAuthorizationCodeIssue) error

	ExchangeOAuthAuthorizationCode(context.Context, OAuthCodeExchange, time.Time) (OAuthTokenMetadata, error)
	RotateOAuthRefreshToken(context.Context, OAuthRefreshExchange, time.Time) (OAuthTokenMetadata, error)
	// RevokeOAuthTokenByHash implements RFC 7009 client-initiated revocation:
	// tokenHash may be either an access or refresh token hash. Per RFC 7009 the
	// endpoint returns success whether or not a matching token was found, but
	// the boolean return tells the caller whether anything was actually revoked
	// (for audit/logging), and it never revokes a token issued to a different
	// client than the one presenting it.
	RevokeOAuthTokenByHash(ctx context.Context, clientID uuid.UUID, tokenHash string) (bool, error)

	ListOAuthConnectedApps(context.Context, uuid.UUID) ([]OAuthConnectedApp, error)
	RevokeOAuthConnectedApp(context.Context, uuid.UUID, uuid.UUID, MutationActor) (int64, error)

	ExpireOAuthArtifacts(context.Context, time.Time, int) (int, error)

	// Registration keys gate POST /oauth/register. A subject keeps at most one
	// active key; SetOAuthRegistrationKey doubles as mint and rotation by
	// atomically revoking the prior active key before inserting the new one.
	SetOAuthRegistrationKey(context.Context, uuid.UUID, string, MutationActor) (OAuthRegistrationKey, error)
	RevokeOAuthRegistrationKey(context.Context, uuid.UUID, MutationActor) error
	GetOAuthRegistrationKeySubject(context.Context, string) (uuid.UUID, error)
	HasOAuthRegistrationKey(context.Context, uuid.UUID) (bool, error)
}

func validateOAuthClientRegistration(input OAuthClientRegistration) error {
	if err := validateOAuthClientText(input.Name); err != nil {
		return err
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return fmt.Errorf("%w: client_id is required", ErrInvalidOAuthClient)
	}
	switch input.ClientType {
	case OAuthClientConfidential:
		if input.ClientSecretHash == "" {
			return fmt.Errorf("%w: confidential clients require a secret hash", ErrInvalidOAuthClient)
		}
	case OAuthClientPublic:
		if input.ClientSecretHash != "" {
			return fmt.Errorf("%w: public clients must not have a secret hash", ErrInvalidOAuthClient)
		}
	default:
		return fmt.Errorf("%w: client type must be confidential or public", ErrInvalidOAuthClient)
	}
	if err := validateOAuthRedirectURIs(input.RedirectURIs); err != nil {
		return err
	}
	return validateOAuthScopes(input.AllowedScopes)
}

func validateOAuthClientText(name string) error {
	if strings.TrimSpace(name) != name || name == "" || len(name) > maxOAuthClientNameLength {
		return fmt.Errorf("%w: name must be trimmed and between 1 and %d characters", ErrInvalidOAuthClient, maxOAuthClientNameLength)
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: name contains control characters", ErrInvalidOAuthClient)
		}
	}
	return nil
}

// validateOAuthRedirectURIs enforces exact-match-only registration: every URI
// must be absolute HTTPS, or explicit HTTP loopback for local development,
// matching the rule already used for FUSED_AUTH_PUBLIC_URL.
func validateOAuthRedirectURIs(uris []string) error {
	if len(uris) == 0 || len(uris) > maxOAuthRedirectURIs {
		return fmt.Errorf("%w: between 1 and %d redirect URIs are required", ErrInvalidOAuthClient, maxOAuthRedirectURIs)
	}
	seen := make(map[string]struct{}, len(uris))
	for _, raw := range uris {
		if _, duplicate := seen[raw]; duplicate {
			return fmt.Errorf("%w: duplicate redirect URI", ErrInvalidOAuthClient)
		}
		seen[raw] = struct{}{}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("%w: invalid redirect URI %q", ErrInvalidOAuthClient, raw)
		}
		if parsed.Scheme == "https" {
			continue
		}
		if parsed.Scheme == "http" && isLoopbackRedirectHost(parsed.Hostname()) {
			continue
		}
		return fmt.Errorf("%w: redirect URI %q must be HTTPS or an explicit loopback address", ErrInvalidOAuthClient, raw)
	}
	return nil
}

func isLoopbackRedirectHost(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

func validateOAuthScopes(scopes []string) error {
	if len(scopes) == 0 || len(scopes) > maxOAuthScopeCount {
		return fmt.Errorf("%w: between 1 and %d scopes are required", ErrInvalidOAuthClient, maxOAuthScopeCount)
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if err := accesscontrol.ValidatePermission(accesscontrol.Permission(scope)); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidOAuthClient, err)
		}
		if _, duplicate := seen[scope]; duplicate {
			return fmt.Errorf("%w: duplicate scope %q", ErrInvalidOAuthClient, scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}
