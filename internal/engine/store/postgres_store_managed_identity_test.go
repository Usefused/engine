package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresManagedLoginBindsExactInvitationAndIssuesExistingCredential(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	_, _, actor := bootstrapUserTest(t, ctx, repository, "managed-login")
	created, err := repository.CreateUser(ctx, CreateUserInput{
		Email: "Invited.Person@Example.com", DisplayName: "Invited Person", Actor: actor,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	transaction := createVerifiedManagedLoginFixture(t, ctx, pool, repository, "poll-hash-1", "subject-1", "invited.person@example.com")
	now := time.Now().UTC()
	credential, err := repository.ConsumeManagedLoginAssertion(ctx, transaction.ID, transaction.PollSecretHash, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ConsumeManagedLoginAssertion: %v", err)
	}
	if credential.UserID != created.User.ID || credential.RawKey == "" || credential.AuthorizationRevision <= created.AuthorizationRevision {
		t.Fatalf("unexpected managed credential: %#v", credential)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(credential.RawKey))
	if err != nil || principal.SubjectID != created.User.ID || principal.CredentialID != credential.CredentialID {
		t.Fatalf("managed principal = %#v, %v", principal, err)
	}

	var status, keyHash, source, authMethod, transactionState string
	var storedLogoutDEK, storedLogoutToken string
	var metadata []byte
	var clearedIdentity bool
	if err := pool.QueryRow(ctx, `SELECT status FROM fused_subjects WHERE id = $1`, created.User.ID).Scan(&status); err != nil {
		t.Fatalf("load activated user: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT key_hash, source, auth_method FROM fused_control_credentials WHERE id = $1`, credential.CredentialID).Scan(&keyHash, &source, &authMethod); err != nil {
		t.Fatalf("load credential hash: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT metadata FROM fused_audit_events WHERE action = 'user.managed_login' AND actor_credential_id = $1`, credential.CredentialID).Scan(&metadata); err != nil {
		t.Fatalf("load managed login audit: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT state, issuer IS NULL AND external_subject IS NULL AND verified_email IS NULL
			AND encrypted_logout_token IS NULL
		FROM fused_managed_login_transactions WHERE id = $1
	`, transaction.ID).Scan(&transactionState, &clearedIdentity); err != nil {
		t.Fatalf("load consumed transaction: %v", err)
	}
	if status != string(UserStatusActive) || keyHash == credential.RawKey || source != "managed_login" || authMethod != "email_code" || transactionState != "consumed" || !clearedIdentity {
		t.Fatalf("managed persistence status/hash/state/cleared = %q/%q/%q/%v", status, keyHash, transactionState, clearedIdentity)
	}
	if err := pool.QueryRow(ctx, `
		SELECT encrypted_dek, encrypted_logout_token
		FROM fused_browser_logout_contexts WHERE credential_id = $1
	`, credential.CredentialID).Scan(&storedLogoutDEK, &storedLogoutToken); err != nil {
		t.Fatalf("load managed logout context: %v", err)
	}
	if storedLogoutDEK != "wrapped-logout-dek" || storedLogoutToken != "encrypted-logout-token" {
		t.Fatalf("managed logout context = %q/%q", storedLogoutDEK, storedLogoutToken)
	}
	for _, secret := range []string{credential.RawKey, "invited.person@example.com", "subject-1"} {
		if strings.Contains(string(metadata), secret) {
			t.Fatalf("managed login audit leaked identity or credential: %s", metadata)
		}
	}
	if _, err := repository.ConsumeManagedLoginAssertion(ctx, transaction.ID, transaction.PollSecretHash, now, now.Add(time.Hour)); !errors.Is(err, ErrManagedLoginUnavailable) {
		t.Fatalf("managed login replay error = %v", err)
	}
	logout, err := repository.RevokeBrowserSession(ctx, accesscontrol.Actor{
		SubjectID: credential.UserID, CredentialID: credential.CredentialID,
	}, now)
	if err != nil || logout.EncryptedDEK != "wrapped-logout-dek" || logout.EncryptedLogoutToken != "encrypted-logout-token" {
		t.Fatalf("managed browser logout context = %#v, %v", logout, err)
	}
	var logoutContextExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fused_browser_logout_contexts WHERE credential_id = $1)`, credential.CredentialID).Scan(&logoutContextExists); err != nil || logoutContextExists {
		t.Fatalf("managed logout context remained after revoke: exists=%v err=%v", logoutContextExists, err)
	}

	// A durable external subject wins over later email claims, preventing an
	// email change at the provider from silently rebinding local authorization.
	second := createVerifiedManagedLoginFixture(t, ctx, pool, repository, "poll-hash-2", "subject-1", "different@example.com")
	secondCredential, err := repository.ConsumeManagedLoginAssertion(ctx, second.ID, second.PollSecretHash, now, now.Add(time.Hour))
	if err != nil || secondCredential.UserID != created.User.ID {
		t.Fatalf("existing external binding = %#v, %v", secondCredential, err)
	}
	expired, err := repository.ExpireBrowserLogoutContexts(ctx, now.Add(2*time.Hour), 10)
	if err != nil || expired != 1 {
		t.Fatalf("ExpireBrowserLogoutContexts = %d, %v", expired, err)
	}
}

func TestPostgresManagedLoginDeniesUninvitedAndCrossInstallationIdentity(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, _, _ = bootstrapUserTest(t, ctx, repository, "managed-denial")

	uninvited := createVerifiedManagedLoginFixture(t, ctx, pool, repository, "poll-hash-uninvited", "subject-uninvited", "nobody@example.com")
	now := time.Now().UTC()
	if _, err := repository.ConsumeManagedLoginAssertion(ctx, uninvited.ID, uninvited.PollSecretHash, now, now.Add(time.Hour)); !errors.Is(err, ErrManagedIdentityDenied) {
		t.Fatalf("uninvited identity error = %v", err)
	}

	transaction := createManagedLoginFixture(t, ctx, pool, repository, "poll-hash-cross-install")
	claimed, err := repository.ClaimManagedLoginExchange(ctx, transaction.ID, transaction.PollSecretHash, now)
	if err != nil {
		t.Fatalf("ClaimManagedLoginExchange: %v", err)
	}
	identity := managedIdentityFixture(claimed, "subject-cross-install", "nobody@example.com", now)
	identity.InstallationID = uuid.New()
	if err := repository.SaveManagedLoginAssertion(ctx, transaction.ID, transaction.PollSecretHash, identity, now); !errors.Is(err, ErrManagedLoginUnavailable) {
		t.Fatalf("cross-install assertion error = %v", err)
	}
}

func createVerifiedManagedLoginFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *postgresStore,
	pollHash, externalSubject, email string,
) ManagedLoginTransaction {
	t.Helper()
	transaction := createManagedLoginFixture(t, ctx, pool, repository, pollHash)
	now := time.Now().UTC()
	claimed, err := repository.ClaimManagedLoginExchange(ctx, transaction.ID, pollHash, now)
	if err != nil {
		t.Fatalf("ClaimManagedLoginExchange: %v", err)
	}
	identity := managedIdentityFixture(claimed, externalSubject, email, now)
	if err := repository.SaveManagedLoginAssertion(ctx, transaction.ID, pollHash, identity, now); err != nil {
		t.Fatalf("SaveManagedLoginAssertion: %v", err)
	}
	return claimed
}

func createManagedLoginFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *postgresStore,
	pollHash string,
) ManagedLoginTransaction {
	t.Helper()
	var installationID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT installation_id FROM fused_engine_installation WHERE singleton_key = 1`).Scan(&installationID); err != nil {
		t.Fatalf("load Engine installation: %v", err)
	}
	transaction := ManagedLoginTransaction{
		ID: uuid.New(), RegistryTransactionID: uuid.New(), Purpose: "browser_login",
		PollSecretHash: pollHash, EnrollmentRef: uuid.NewString(),
		EncryptedDEK: "encrypted-dek", EncryptedRegistryVerifier: "encrypted-verifier",
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	if err := repository.CreateManagedLoginTransaction(ctx, transaction); err != nil {
		t.Fatalf("CreateManagedLoginTransaction: %v", err)
	}
	transaction.InstallationID = installationID
	return transaction
}

func managedIdentityFixture(transaction ManagedLoginTransaction, externalSubject, email string, now time.Time) VerifiedManagedIdentity {
	return VerifiedManagedIdentity{
		AccountID: transaction.AccountID, InstallationID: transaction.InstallationID,
		Purpose: transaction.Purpose, Provider: "logto", Issuer: "https://tenant.logto.test/oidc",
		ExternalSubject: externalSubject, VerifiedEmail: email, DisplayName: "Managed User",
		AuthMethod: "email_code", EnrollmentRef: transaction.EnrollmentRef,
		AuthenticatedAt: now, AssertionExpires: transaction.ExpiresAt,
		LogoutEncryptedDEK: "wrapped-logout-dek", EncryptedLogoutToken: "encrypted-logout-token",
		LogoutExpiresAt: now.Add(time.Hour),
	}
}
