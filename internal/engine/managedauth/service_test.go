package managedauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type managedAuthStoreFixture struct {
	accountID, installationID uuid.UUID
	transaction               store.ManagedLoginTransaction
	identity                  store.VerifiedManagedIdentity
	released, rejected        bool
	credential                store.ManagedSessionCredential
}

func (f *managedAuthStoreFixture) CreateManagedLoginTransaction(_ context.Context, transaction store.ManagedLoginTransaction) error {
	transaction.AccountID, transaction.InstallationID = f.accountID, f.installationID
	f.transaction = transaction
	return nil
}

func (f *managedAuthStoreFixture) ClaimManagedLoginExchange(_ context.Context, id uuid.UUID, hash string, _ time.Time) (store.ManagedLoginTransaction, error) {
	if id != f.transaction.ID || hash != f.transaction.PollSecretHash {
		return store.ManagedLoginTransaction{}, store.ErrManagedLoginPending
	}
	f.transaction.State = store.ManagedLoginStateExchanging
	return f.transaction, nil
}

func (f *managedAuthStoreFixture) ReleaseManagedLoginExchange(context.Context, uuid.UUID, string, time.Time) error {
	f.released = true
	return nil
}

func (f *managedAuthStoreFixture) RejectManagedLoginTransaction(context.Context, uuid.UUID, string, time.Time) error {
	f.rejected = true
	return nil
}

func (f *managedAuthStoreFixture) SaveManagedLoginAssertion(_ context.Context, _ uuid.UUID, _ string, identity store.VerifiedManagedIdentity, _ time.Time) error {
	f.identity = identity
	f.transaction.State = store.ManagedLoginStateVerified
	return nil
}

func (f *managedAuthStoreFixture) ConsumeManagedLoginAssertion(context.Context, uuid.UUID, string, time.Time, time.Time) (store.ManagedSessionCredential, error) {
	return f.credential, nil
}

func (*managedAuthStoreFixture) ExpireManagedLoginTransactions(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type managedAuthRegistryFixture struct {
	transactionID uuid.UUID
	expiresAt     time.Time
	verifier      string
	enrollmentRef string
	assertion     sandbox.ManagedIdentityAssertion
	exchangeErr   error
}

func (f *managedAuthRegistryFixture) CreateManagedLoginTransaction(_ context.Context, verifier, enrollmentRef string) (sandbox.ManagedLoginTransaction, error) {
	f.verifier, f.enrollmentRef = verifier, enrollmentRef
	return sandbox.ManagedLoginTransaction{
		TransactionID: f.transactionID, VerificationURL: "https://auth.usefused.test/auth/t/" + f.transactionID.String(),
		ExpiresAt: f.expiresAt,
	}, nil
}

func (f *managedAuthRegistryFixture) ExchangeManagedLoginTransaction(_ context.Context, id uuid.UUID, verifier string) (sandbox.ManagedIdentityAssertion, error) {
	if id != f.transactionID || verifier != f.verifier {
		return sandbox.ManagedIdentityAssertion{}, errors.New("unexpected Registry exchange input")
	}
	return f.assertion, f.exchangeErr
}

type managedAuthRevisionFixture struct{ revision int64 }

func (f *managedAuthRevisionFixture) SetRevision(revision int64) bool {
	f.revision = revision
	return true
}

func TestServiceStartAndPollPersistEncryptedVerifierAndIssueLocalCredential(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	accountID, installationID, registryTransactionID := uuid.New(), uuid.New(), uuid.New()
	repository := &managedAuthStoreFixture{
		accountID: accountID, installationID: installationID,
		credential: store.ManagedSessionCredential{
			UserID: uuid.New(), CredentialID: uuid.New(), RawKey: "fsk_browser_secret",
			ExpiresAt: now.Add(defaultSessionTTL), AuthorizationRevision: 7, AuthMethod: "email_code",
		},
	}
	registry := &managedAuthRegistryFixture{transactionID: registryTransactionID, expiresAt: now.Add(10 * time.Minute)}
	revisions := &managedAuthRevisionFixture{}
	service, err := NewService(repository, registry, revisions, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.now = func() time.Time { return now }

	started, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManagedAuthStart(t, started, repository, registry)
	registry.assertion = validManagedAssertion(repository, registry, now)
	credential, err := service.Poll(t.Context(), started.TransactionID, started.PollToken)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	assertManagedAuthCompletion(t, credential, revisions, repository)
}

func assertManagedAuthStart(t *testing.T, started StartResult, repository *managedAuthStoreFixture, registry *managedAuthRegistryFixture) {
	t.Helper()
	if started.TransactionID == uuid.Nil || started.PollToken == "" || registry.enrollmentRef != started.TransactionID.String() {
		t.Fatalf("unexpected start result: %#v enrollment=%q", started, registry.enrollmentRef)
	}
	if repository.transaction.PollSecretHash == started.PollToken || repository.transaction.EncryptedRegistryVerifier == registry.verifier {
		t.Fatal("managed login secrets were stored without hashing/encryption")
	}
}

func assertManagedAuthCompletion(t *testing.T, credential store.ManagedSessionCredential, revisions *managedAuthRevisionFixture, repository *managedAuthStoreFixture) {
	t.Helper()
	if credential.RawKey != "fsk_browser_secret" || revisions.revision != 7 || repository.identity.ExternalSubject != "user_123" {
		t.Fatalf("unexpected managed login completion: credential=%#v revision=%d identity=%#v", credential, revisions.revision, repository.identity)
	}
}

func TestRegistryTransactionAllowsHTTPOnlyForExplicitDevelopmentLoopback(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		environment string
		url         string
		valid       bool
	}{
		{name: "production HTTPS", url: "https://auth.usefused.test/auth/t/id", valid: true},
		{name: "development localhost", environment: "development", url: "http://localhost:8080/auth/t/id", valid: true},
		{name: "development IPv4", environment: "development", url: "http://127.0.0.1:8080/auth/t/id", valid: true},
		{name: "development IPv6", environment: "development", url: "http://[::1]:8080/auth/t/id", valid: true},
		{name: "production loopback", url: "http://localhost:8080/auth/t/id"},
		{name: "abbreviated environment", environment: "dev", url: "http://localhost:8080/auth/t/id"},
		{name: "development remote HTTP", environment: "development", url: "http://auth.usefused.test/auth/t/id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FUSED_ENV", test.environment)
			transaction := sandbox.ManagedLoginTransaction{
				TransactionID: uuid.New(), VerificationURL: test.url, ExpiresAt: now.Add(10 * time.Minute),
			}
			if actual := validRegistryTransaction(transaction, now); actual != test.valid {
				t.Fatalf("validRegistryTransaction() = %v, want %v", actual, test.valid)
			}
		})
	}
}

func TestServicePollTreatsUnavailableRegistryTransactionAsPending(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	repository, registry, service, started := startedManagedAuthFixture(t, now)
	registry.exchangeErr = sandbox.ManagedIdentityRegistryError{Status: 404, Code: "transaction_unavailable"}
	_, err := service.Poll(t.Context(), started.TransactionID, started.PollToken)
	if !errors.Is(err, store.ErrManagedLoginPending) || !repository.released {
		t.Fatalf("Poll error/release = %v/%v, want pending/true", err, repository.released)
	}
}

func TestServicePollRejectsCrossInstallationAssertionAndKeepsSecretsOutOfOTEL(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(t.Context())
		otel.SetTracerProvider(previous)
	})

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	repository, registry, service, started := startedManagedAuthFixture(t, now)
	registry.assertion = validManagedAssertion(repository, registry, now)
	registry.assertion.InstallationID = uuid.New()
	_, err := service.Poll(t.Context(), started.TransactionID, started.PollToken)
	if !errors.Is(err, store.ErrManagedIdentityDenied) || !repository.rejected {
		t.Fatalf("Poll error/rejected = %v/%v, want denied/true", err, repository.rejected)
	}
	assertManagedAuthTelemetryExcludes(t, exporter, started.PollToken, registry.verifier)
}

func startedManagedAuthFixture(t *testing.T, now time.Time) (*managedAuthStoreFixture, *managedAuthRegistryFixture, *Service, StartResult) {
	t.Helper()
	repository := &managedAuthStoreFixture{accountID: uuid.New(), installationID: uuid.New()}
	registry := &managedAuthRegistryFixture{transactionID: uuid.New(), expiresAt: now.Add(10 * time.Minute)}
	service, err := NewService(repository, registry, &managedAuthRevisionFixture{}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.now = func() time.Time { return now }
	started, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return repository, registry, service, started
}

func validManagedAssertion(repository *managedAuthStoreFixture, registry *managedAuthRegistryFixture, now time.Time) sandbox.ManagedIdentityAssertion {
	return sandbox.ManagedIdentityAssertion{
		SchemaVersion: 1, TransactionID: registry.transactionID,
		AccountID: repository.accountID, InstallationID: repository.installationID,
		Purpose: BrowserLoginPurpose, Provider: "logto", Issuer: "https://tenant.logto.test/oidc",
		ExternalSubject: "user_123", VerifiedEmail: "person@example.com", DisplayName: "Person",
		AuthMethod: "email_code", EnrollmentRef: repository.transaction.EnrollmentRef,
		AuthenticatedAt: now, ExpiresAt: registry.expiresAt,
	}
}

func assertManagedAuthTelemetryExcludes(t *testing.T, exporter *tracetest.InMemoryExporter, forbidden ...string) {
	t.Helper()
	var values []string
	for _, span := range exporter.GetSpans() {
		values = append(values, span.Name)
		for _, item := range span.Attributes {
			values = append(values, string(item.Key), fmt.Sprint(item.Value.AsInterface()))
		}
	}
	joined := strings.Join(values, " ")
	for _, secret := range forbidden {
		if strings.Contains(joined, secret) {
			t.Fatalf("managed identity secret leaked into OTEL: %q", secret)
		}
	}
}
