package managedauthbroker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const (
	accessTokenTTL  = 5 * time.Minute
	refreshTokenTTL = 30 * 24 * time.Hour

	accessTokenPrefix  = "fmaat_"
	refreshTokenPrefix = "fmart_"
)

// managedAuthScope is fixed rather than negotiated: an installation
// credential only ever grants the ability to use Fused's pre-registered
// OAuth apps for connecting services, never any of the broader permissions
// a Fused user's own delegated oauthprovider token can carry.
var managedAuthScope = []string{"managed_auth:connect"}

var ErrInvalidTicket = errors.New("managed-auth enrollment ticket was rejected by Registry")

// EnrollmentIdentity carries Registry-verified account ownership of an Engine installation.
type EnrollmentIdentity struct {
	AccountID      uuid.UUID `json:"account_id"`
	InstallationID uuid.UUID `json:"installation_id"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// TicketVerifier redeems a Registry-issued enrollment ticket. The concrete
// implementation calls Registry over HTTP; tests can substitute a fake.
type TicketVerifier interface {
	VerifyTicket(ctx context.Context, ticket string) (EnrollmentIdentity, error)
}

type Service struct {
	store    *Store
	verifier TicketVerifier
	now      func() time.Time
}

func NewService(store *Store, verifier TicketVerifier) (*Service, error) {
	if store == nil || verifier == nil {
		return nil, errors.New("invalid managed-auth broker configuration")
	}
	return &Service{store: store, verifier: verifier, now: time.Now}, nil
}

type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Scope        []string
}

// Enroll redeems a Registry enrollment ticket and issues a fresh installation
// credential for the verified installation without affecting sibling installations.
func (s *Service) Enroll(ctx context.Context, ticket string) (TokenResponse, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.managedauthbroker.enroll")
	defer span.End()
	identity, err := s.verifyAuthority(ctx, ticket)
	// Both identifiers are mandatory even for alternate verifier implementations.
	if err != nil || identity.AccountID == uuid.Nil || identity.InstallationID == uuid.Nil {
		span.SetAttributes(attribute.String("outcome", "invalid_ticket"))
		return TokenResponse{}, ErrInvalidTicket
	}
	result, err := s.issue(ctx, identity)
	span.SetAttributes(attribute.String("outcome", outcomeOf(err)))
	return result, err
}

// Refresh generates replacement material before atomically consuming the current refresh hash.
func (s *Service) Refresh(ctx context.Context, refreshToken, ticket string) (TokenResponse, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.managedauthbroker.refresh")
	defer span.End()
	identity, err := s.verifyAuthority(ctx, ticket)
	// A refresh credential cannot renew authority after Registry withdraws eligibility.
	if err != nil {
		return TokenResponse{}, err
	}
	result, err := newTokenResponse()
	// Entropy failure must not consume the caller's current credential.
	if err != nil {
		return TokenResponse{}, err
	}
	now := s.now()
	err = s.store.RotateRefreshToken(ctx, hashCredential(refreshToken), hashCredential(result.AccessToken), hashCredential(result.RefreshToken), identity, identity.ExpiresAt, now.Add(refreshTokenTTL))
	span.SetAttributes(attribute.String("outcome", outcomeOf(err)))
	// Keep refresh rejection distinct from transient storage failures.
	if errors.Is(err, ErrTokenInvalid) {
		return TokenResponse{}, ErrInvalidTicket
	}
	// Never expose replacement tokens until they have committed.
	if err != nil {
		return TokenResponse{}, err
	}
	result.ExpiresIn = int64(identity.ExpiresAt.Sub(now).Seconds())
	return result, nil
}

// outcomeOf records a bounded result without exposing credentials or database diagnostics.
func outcomeOf(err error) string {
	if err != nil {
		return "failed"
	}
	return "succeeded"
}

// issue commits a fresh token family for exactly the Registry-verified installation.
func (s *Service) issue(ctx context.Context, identity EnrollmentIdentity) (TokenResponse, error) {
	result, err := newTokenResponse()
	// Generate both credentials before replacing any existing enrollment.
	if err != nil {
		return TokenResponse{}, err
	}
	now := s.now()
	err = s.store.IssueInstallation(ctx, identity, managedAuthScope, hashCredential(result.AccessToken), hashCredential(result.RefreshToken), uuid.New(), identity.ExpiresAt, now.Add(refreshTokenTTL))
	// Return bearer material only after it is durable at the broker.
	if err != nil {
		return TokenResponse{}, fmt.Errorf("issue managed-auth installation credential: %w", err)
	}
	result.ExpiresIn = int64(identity.ExpiresAt.Sub(now).Seconds())
	return result, nil
}

// newTokenResponse creates a complete pair so partial entropy failures never mutate stored authority.
func newTokenResponse() (TokenResponse, error) {
	access, _, err := randomCredential(accessTokenPrefix)
	// A failed access-token draw cannot produce a usable pair.
	if err != nil {
		return TokenResponse{}, err
	}
	refresh, _, err := randomCredential(refreshTokenPrefix)
	// A failed refresh-token draw leaves the existing grant untouched.
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{AccessToken: access, RefreshToken: refresh, ExpiresIn: int64(accessTokenTTL.Seconds()), Scope: managedAuthScope}, nil
}

func randomCredential(prefix string) (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = prefix + base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashCredential(raw), nil
}

func hashCredential(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// verifyAuthority limits a broker grant to the Registry ticket's original approval deadline.
// Delaying redemption cannot extend the five-minute revocation bound.
func (s *Service) verifyAuthority(ctx context.Context, ticket string) (EnrollmentIdentity, error) {
	identity, err := s.verifier.VerifyTicket(ctx, ticket)
	// Missing identity or an expired approval is never substituted with a fresh local lifetime.
	if err != nil || identity.AccountID == uuid.Nil || identity.InstallationID == uuid.Nil || !identity.ExpiresAt.After(s.now().Add(time.Second)) {
		return EnrollmentIdentity{}, ErrInvalidTicket
	}
	// Even an incorrectly configured authority response cannot exceed this broker's maximum lease.
	if ceiling := s.now().Add(accessTokenTTL); identity.ExpiresAt.After(ceiling) {
		identity.ExpiresAt = ceiling
	}
	return identity, nil
}

// Revoke withdraws this credential pair without requiring a still-valid Registry license.
func (s *Service) Revoke(ctx context.Context, refreshToken string) error {
	return s.store.Revoke(ctx, hashCredential(refreshToken))
}
