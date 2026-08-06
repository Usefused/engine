package managedauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const (
	BrowserLoginPurpose = "browser_login"
	defaultSessionTTL   = 8 * time.Hour
	maximumLogoutTTL    = 24 * time.Hour
	cleanupBatchSize    = 500
)

type Registry interface {
	CreateManagedLoginTransaction(context.Context, string, string) (sandbox.ManagedLoginTransaction, error)
	ExchangeManagedLoginTransaction(context.Context, uuid.UUID, string) (sandbox.ManagedIdentityAssertion, error)
}

type RevisionSink interface {
	SetRevision(int64) bool
}

type StartResult struct {
	TransactionID   uuid.UUID
	PollToken       string
	VerificationURL string
	ExpiresAt       time.Time
}

type Service struct {
	store      store.ManagedIdentityStore
	registry   Registry
	revisions  RevisionSink
	masterKey  []byte
	now        func() time.Time
	sessionTTL time.Duration
}

func NewService(repository store.ManagedIdentityStore, registry Registry, revisions RevisionSink, masterKey []byte) (*Service, error) {
	if repository == nil || registry == nil || revisions == nil || len(masterKey) != 32 {
		return nil, errors.New("invalid managed identity service configuration")
	}
	return &Service{
		store: repository, registry: registry, revisions: revisions,
		masterKey: append([]byte(nil), masterKey...), now: time.Now, sessionTTL: defaultSessionTTL,
	}, nil
}

func (s *Service) Start(ctx context.Context) (StartResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.login.start")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"), attribute.String("identity.purpose", BrowserLoginPurpose))
	result, transaction, err := s.prepareLoginTransaction(ctx)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return StartResult{}, err
	}
	if err := s.store.CreateManagedLoginTransaction(ctx, transaction); err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return StartResult{}, err
	}
	span.SetAttributes(attribute.String("outcome", "started"))
	return result, nil
}

func (s *Service) prepareLoginTransaction(ctx context.Context) (StartResult, store.ManagedLoginTransaction, error) {
	id := uuid.New()
	pollToken, err := randomToken(32)
	if err != nil {
		return StartResult{}, store.ManagedLoginTransaction{}, err
	}
	registryVerifier, err := randomToken(32)
	if err != nil {
		return StartResult{}, store.ManagedLoginTransaction{}, err
	}
	// Polling can continue on any node sharing the Engine database, while the
	// Registry verifier remains unreadable without the Engine master key.
	encryptedDEK, encryptedVerifier, err := encryptRegistryVerifier(s.masterKey, registryVerifier)
	if err != nil {
		return StartResult{}, store.ManagedLoginTransaction{}, err
	}
	registryTransaction, err := s.registry.CreateManagedLoginTransaction(ctx, registryVerifier, id.String())
	if err != nil {
		return StartResult{}, store.ManagedLoginTransaction{}, store.ErrManagedLoginUnavailable
	}
	if !validRegistryTransaction(registryTransaction, s.now().UTC()) {
		return StartResult{}, store.ManagedLoginTransaction{}, store.ErrManagedLoginUnavailable
	}
	local := store.ManagedLoginTransaction{
		ID: id, RegistryTransactionID: registryTransaction.TransactionID,
		Purpose: BrowserLoginPurpose, PollSecretHash: hashSecret(pollToken), EnrollmentRef: id.String(),
		EncryptedDEK: encryptedDEK, EncryptedRegistryVerifier: encryptedVerifier,
		State: store.ManagedLoginStatePending, ExpiresAt: registryTransaction.ExpiresAt,
	}
	result := StartResult{
		TransactionID: id, PollToken: pollToken,
		VerificationURL: registryTransaction.VerificationURL, ExpiresAt: registryTransaction.ExpiresAt,
	}
	return result, local, nil
}

func (s *Service) Poll(ctx context.Context, id uuid.UUID, pollToken string) (store.ManagedSessionCredential, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.login.consume")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"), attribute.String("identity.purpose", BrowserLoginPurpose))
	if id == uuid.Nil || !validPollToken(pollToken) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return store.ManagedSessionCredential{}, store.ErrManagedLoginUnavailable
	}
	pollHash, now := hashSecret(pollToken), s.now().UTC()
	transaction, err := s.store.ClaimManagedLoginExchange(ctx, id, pollHash, now)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", managedLoginOutcome(err)))
		return store.ManagedSessionCredential{}, err
	}
	if transaction.State != store.ManagedLoginStateVerified {
		if err := s.exchangeAndSave(ctx, transaction, pollHash, now); err != nil {
			span.SetAttributes(attribute.String("outcome", managedLoginOutcome(err)))
			return store.ManagedSessionCredential{}, err
		}
	}
	credential, err := s.store.ConsumeManagedLoginAssertion(ctx, id, pollHash, now, now.Add(s.sessionTTL))
	if err != nil {
		span.SetAttributes(attribute.String("outcome", managedLoginOutcome(err)))
		return store.ManagedSessionCredential{}, err
	}
	s.revisions.SetRevision(credential.AuthorizationRevision)
	span.SetAttributes(
		attribute.String("outcome", "authenticated"),
		attribute.String("identity.auth_method", credential.AuthMethod),
	)
	return credential, nil
}

func (s *Service) exchangeAndSave(ctx context.Context, transaction store.ManagedLoginTransaction, pollHash string, now time.Time) error {
	verifier, err := decryptRegistryVerifier(s.masterKey, transaction)
	if err != nil {
		return store.ErrManagedLoginUnavailable
	}
	assertion, err := s.registry.ExchangeManagedLoginTransaction(ctx, transaction.RegistryTransactionID, verifier)
	if err != nil {
		_ = s.store.ReleaseManagedLoginExchange(ctx, transaction.ID, pollHash, now)
		if sandbox.IsManagedLoginPending(err) {
			return store.ErrManagedLoginPending
		}
		return store.ErrManagedLoginUnavailable
	}
	identity, err := verifiedIdentity(s.masterKey, transaction, assertion, now)
	if err != nil {
		_ = s.store.RejectManagedLoginTransaction(ctx, transaction.ID, pollHash, now)
		return err
	}
	return s.store.SaveManagedLoginAssertion(ctx, transaction.ID, pollHash, identity, now)
}

func verifiedIdentity(masterKey []byte, transaction store.ManagedLoginTransaction, assertion sandbox.ManagedIdentityAssertion, now time.Time) (store.VerifiedManagedIdentity, error) {
	if !assertionMatchesTransaction(transaction, assertion) || !validAssertionClaims(transaction, assertion, now) {
		return store.VerifiedManagedIdentity{}, store.ErrManagedIdentityDenied
	}
	identity := store.VerifiedManagedIdentity{
		AccountID: assertion.AccountID, InstallationID: assertion.InstallationID,
		Purpose: assertion.Purpose, Provider: assertion.Provider, Issuer: assertion.Issuer,
		ExternalSubject: assertion.ExternalSubject, VerifiedEmail: assertion.VerifiedEmail,
		DisplayName: assertion.DisplayName, AuthMethod: assertion.AuthMethod,
		EnrollmentRef: assertion.EnrollmentRef, AuthenticatedAt: assertion.AuthenticatedAt,
		AssertionExpires: assertion.ExpiresAt,
	}
	if assertion.LogoutToken == "" {
		return identity, nil
	}
	wrapped, encrypted, err := encryptManagedLogoutToken(masterKey, assertion.LogoutToken)
	if err != nil {
		return store.VerifiedManagedIdentity{}, store.ErrManagedLoginUnavailable
	}
	identity.LogoutEncryptedDEK = wrapped
	identity.EncryptedLogoutToken = encrypted
	identity.LogoutExpiresAt = assertion.LogoutExpiresAt
	return identity, nil
}

func assertionMatchesTransaction(transaction store.ManagedLoginTransaction, assertion sandbox.ManagedIdentityAssertion) bool {
	return assertion.SchemaVersion == 1 && assertion.TransactionID == transaction.RegistryTransactionID &&
		assertion.AccountID == transaction.AccountID && assertion.InstallationID == transaction.InstallationID &&
		assertion.Purpose == BrowserLoginPurpose && assertion.EnrollmentRef == transaction.EnrollmentRef
}

func validAssertionClaims(transaction store.ManagedLoginTransaction, assertion sandbox.ManagedIdentityAssertion, now time.Time) bool {
	return assertion.Provider == "logto" && validAssertionStrings(assertion) &&
		!assertion.AuthenticatedAt.IsZero() && assertion.ExpiresAt.After(now) &&
		!assertion.ExpiresAt.After(transaction.ExpiresAt.Add(time.Second)) && validLogoutClaims(assertion, now)
}

func validLogoutClaims(assertion sandbox.ManagedIdentityAssertion, now time.Time) bool {
	hasToken := assertion.LogoutToken != ""
	hasExpiry := !assertion.LogoutExpiresAt.IsZero()
	if hasToken != hasExpiry {
		return false
	}
	if !hasToken {
		return true
	}
	return boundedValue(assertion.LogoutToken, 256) && assertion.LogoutExpiresAt.After(now) &&
		!assertion.LogoutExpiresAt.After(now.Add(maximumLogoutTTL))
}

func validAssertionStrings(assertion sandbox.ManagedIdentityAssertion) bool {
	return boundedValue(assertion.Issuer, 512) && boundedValue(assertion.ExternalSubject, 512) &&
		boundedValue(assertion.VerifiedEmail, 320) && len(assertion.DisplayName) <= 256 &&
		validAuthMethod(assertion.AuthMethod)
}

func boundedValue(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}

func validAuthMethod(method string) bool {
	switch method {
	case "email_code", "google", "microsoft", "enterprise_sso", "oidc":
		return true
	default:
		return false
	}
}

func validRegistryTransaction(transaction sandbox.ManagedLoginTransaction, now time.Time) bool {
	parsed, err := url.Parse(transaction.VerificationURL)
	return err == nil && transaction.TransactionID != uuid.Nil && validVerificationURL(parsed) &&
		transaction.ExpiresAt.After(now) && transaction.ExpiresAt.Before(now.Add(16*time.Minute))
}

func validVerificationURL(parsed *url.URL) bool {
	if parsed == nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	// Local browser redirects need HTTP during development, but accepting a
	// non-loopback host would send transaction state over a cleartext network.
	return os.Getenv("FUSED_ENV") == "development" && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func encryptRegistryVerifier(masterKey []byte, verifier string) (string, string, error) {
	wrapped, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return "", "", err
	}
	ciphertext, err := store.EncryptWithDEK(dek, verifier)
	return wrapped, ciphertext, err
}

func encryptManagedLogoutToken(masterKey []byte, token string) (string, string, error) {
	wrapped, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return "", "", err
	}
	ciphertext, err := store.EncryptWithDEK(dek, token)
	return wrapped, ciphertext, err
}

func decryptRegistryVerifier(masterKey []byte, transaction store.ManagedLoginTransaction) (string, error) {
	dek, err := store.UnwrapDEK(masterKey, transaction.EncryptedDEK)
	if err != nil {
		return "", err
	}
	return store.DecryptWithDEK(dek, transaction.EncryptedRegistryVerifier)
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validPollToken(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func hashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func managedLoginOutcome(err error) string {
	switch {
	case errors.Is(err, store.ErrManagedLoginPending):
		return "pending"
	case errors.Is(err, store.ErrManagedIdentityDenied):
		return "denied"
	default:
		return "unavailable"
	}
}

func (s *Service) StartCleanupWorker(ctx context.Context, interval time.Duration) {
	if interval < 10*time.Second || interval > time.Hour {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.cleanup(ctx)
			}
		}
	}()
}

func (s *Service) cleanup(ctx context.Context) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.login.cleanup")
	defer span.End()
	now := s.now().UTC()
	expired, loginErr := s.store.ExpireManagedLoginTransactions(ctx, now, cleanupBatchSize)
	logoutExpired, logoutErr := s.store.ExpireBrowserLogoutContexts(ctx, now, cleanupBatchSize)
	if loginErr != nil || logoutErr != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		slog.ErrorContext(ctx, "Managed login cleanup failed")
		return
	}
	span.SetAttributes(
		attribute.String("outcome", "completed"),
		attribute.Int64("identity.transactions_expired", expired),
		attribute.Int64("identity.logout_contexts_expired", logoutExpired),
	)
}
