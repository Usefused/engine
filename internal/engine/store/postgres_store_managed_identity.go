package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"
)

const managedLoginExchangeLease = 30 * time.Second

func (s *postgresStore) CreateManagedLoginTransaction(ctx context.Context, transaction ManagedLoginTransaction) error {
	command, err := s.db.Exec(ctx, `
		INSERT INTO fused_managed_login_transactions (
			id, registry_transaction_id, account_id, installation_id, purpose,
			poll_secret_hash, enrollment_ref, encrypted_dek,
			encrypted_registry_verifier, state, expires_at
		)
		SELECT $1, $2, workspace.account_id, installation.installation_id, $3,
			$4, $5, $6, $7, 'pending', $8
		FROM fused_workspaces workspace
		CROSS JOIN fused_engine_installation installation
		WHERE workspace.singleton_key = 1 AND installation.singleton_key = 1
	`, transaction.ID, transaction.RegistryTransactionID, transaction.Purpose,
		transaction.PollSecretHash, transaction.EnrollmentRef, transaction.EncryptedDEK,
		transaction.EncryptedRegistryVerifier, transaction.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create managed login transaction: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrManagedLoginUnavailable
	}
	return nil
}

func (s *postgresStore) ClaimManagedLoginExchange(ctx context.Context, id uuid.UUID, pollSecretHash string, at time.Time) (ManagedLoginTransaction, error) {
	// A short database lease lets another Engine node recover a poll abandoned
	// between the Registry exchange and local assertion persistence.
	transaction, err := scanManagedLoginTransaction(s.db.QueryRow(ctx, `
		UPDATE fused_managed_login_transactions
		SET state = CASE WHEN state = 'verified' THEN state ELSE 'exchanging' END,
			exchange_started_at = CASE WHEN state = 'verified' THEN exchange_started_at ELSE $3 END
		WHERE id = $1 AND poll_secret_hash = $2 AND expires_at > $3
			AND (
				state IN ('pending', 'verified')
				OR (state = 'exchanging' AND exchange_started_at < $3 - $4::interval)
			)
		RETURNING id, registry_transaction_id, account_id, installation_id, purpose,
			poll_secret_hash, enrollment_ref, encrypted_dek,
			COALESCE(encrypted_registry_verifier, ''), state,
			COALESCE(provider, ''), COALESCE(issuer, ''), COALESCE(external_subject, ''),
			COALESCE(verified_email, ''), COALESCE(display_name, ''), COALESCE(auth_method, ''),
			COALESCE(authenticated_at, 'epoch'::timestamptz),
			COALESCE(logout_encrypted_dek, ''), COALESCE(encrypted_logout_token, ''),
			COALESCE(logout_expires_at, 'epoch'::timestamptz), expires_at
	`, id, pollSecretHash, at, managedLoginExchangeLease.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return ManagedLoginTransaction{}, ErrManagedLoginPending
	}
	return transaction, err
}

func (s *postgresStore) ReleaseManagedLoginExchange(ctx context.Context, id uuid.UUID, pollSecretHash string, at time.Time) error {
	_, err := s.db.Exec(ctx, `
		UPDATE fused_managed_login_transactions
		SET state = CASE WHEN expires_at <= $3 THEN 'expired' ELSE 'pending' END,
			exchange_started_at = NULL
		WHERE id = $1 AND poll_secret_hash = $2 AND state = 'exchanging'
	`, id, pollSecretHash, at)
	return err
}

func (s *postgresStore) RejectManagedLoginTransaction(ctx context.Context, id uuid.UUID, pollSecretHash string, at time.Time) error {
	_, err := s.db.Exec(ctx, `
		UPDATE fused_managed_login_transactions
		SET state = 'expired', expires_at = LEAST(expires_at, $3), encrypted_dek = '',
			encrypted_registry_verifier = NULL, exchange_started_at = NULL
		WHERE id = $1 AND poll_secret_hash = $2 AND state = 'exchanging'
	`, id, pollSecretHash, at)
	return err
}

func (s *postgresStore) SaveManagedLoginAssertion(ctx context.Context, id uuid.UUID, pollSecretHash string, identity VerifiedManagedIdentity, at time.Time) error {
	command, err := s.db.Exec(ctx, `
		UPDATE fused_managed_login_transactions
		SET state = 'verified', encrypted_dek = '', encrypted_registry_verifier = NULL,
			exchange_started_at = NULL, provider = $4, issuer = $5,
			external_subject = $6, verified_email = $7, display_name = $8,
			auth_method = $9, authenticated_at = $10,
			logout_encrypted_dek = NULLIF($11, ''), encrypted_logout_token = NULLIF($12, ''),
			logout_expires_at = $13
		WHERE id = $1 AND poll_secret_hash = $2 AND state = 'exchanging'
			AND expires_at > $3 AND account_id = $14 AND installation_id = $15
			AND purpose = $16 AND enrollment_ref = $17
	`, id, pollSecretHash, at, identity.Provider, identity.Issuer,
		identity.ExternalSubject, identity.VerifiedEmail, identity.DisplayName,
		identity.AuthMethod, identity.AuthenticatedAt, identity.LogoutEncryptedDEK,
		identity.EncryptedLogoutToken, optionalManagedLogoutExpiry(identity.LogoutExpiresAt),
		identity.AccountID, identity.InstallationID, identity.Purpose, identity.EnrollmentRef)
	if err != nil {
		return fmt.Errorf("save managed identity assertion: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrManagedLoginUnavailable
	}
	return nil
}

func (s *postgresStore) ConsumeManagedLoginAssertion(
	ctx context.Context,
	id uuid.UUID,
	pollSecretHash string,
	at, sessionExpiresAt time.Time,
) (ManagedSessionCredential, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	credential, err := consumeManagedLoginInTransaction(ctx, tx, id, pollSecretHash, at, sessionExpiresAt)
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ManagedSessionCredential{}, fmt.Errorf("commit managed login: %w", err)
	}
	return credential, nil
}

func consumeManagedLoginInTransaction(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
	pollSecretHash string,
	at, sessionExpiresAt time.Time,
) (ManagedSessionCredential, error) {
	transaction, err := lockVerifiedManagedLogin(ctx, tx, id, pollSecretHash, at)
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return ManagedSessionCredential{}, err
	}
	userID, newBinding, err := resolveManagedIdentityUser(ctx, tx, transaction, at)
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	rawKey, err := generateControlCredential()
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	credential, err := insertUserControlCredential(ctx, tx, IssueCredentialInput{
		UserID: userID, Name: "Managed browser " + accesscontrol.CredentialPrefix(rawKey), ExpiresAt: &sessionExpiresAt,
		Source: "managed_login", AuthMethod: transaction.AuthMethod,
	}, rawKey)
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	if err := insertManagedLogoutContext(ctx, tx, credential.ID, transaction, sessionExpiresAt); err != nil {
		return ManagedSessionCredential{}, err
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	if err := auditManagedLogin(ctx, tx, transaction, credential, revision, newBinding); err != nil {
		return ManagedSessionCredential{}, err
	}
	if err := consumeManagedLoginRow(ctx, tx, id, at); err != nil {
		return ManagedSessionCredential{}, err
	}
	return ManagedSessionCredential{
		UserID: userID, CredentialID: credential.ID, RawKey: rawKey,
		ExpiresAt: sessionExpiresAt, AuthorizationRevision: revision,
		AuthMethod: transaction.AuthMethod,
	}, nil
}

func lockVerifiedManagedLogin(ctx context.Context, tx pgx.Tx, id uuid.UUID, pollSecretHash string, at time.Time) (ManagedLoginTransaction, error) {
	transaction, err := scanManagedLoginTransaction(tx.QueryRow(ctx, `
		SELECT transaction.id, transaction.registry_transaction_id,
			transaction.account_id, transaction.installation_id, transaction.purpose,
			transaction.poll_secret_hash, transaction.enrollment_ref, transaction.encrypted_dek,
			COALESCE(transaction.encrypted_registry_verifier, ''), transaction.state,
			transaction.provider, transaction.issuer, transaction.external_subject,
			transaction.verified_email, COALESCE(transaction.display_name, ''), transaction.auth_method,
			transaction.authenticated_at, COALESCE(transaction.logout_encrypted_dek, ''),
			COALESCE(transaction.encrypted_logout_token, ''),
			COALESCE(transaction.logout_expires_at, 'epoch'::timestamptz), transaction.expires_at
		FROM fused_managed_login_transactions transaction
		JOIN fused_workspaces workspace ON workspace.singleton_key = 1
			AND workspace.account_id = transaction.account_id
		JOIN fused_engine_installation installation ON installation.singleton_key = 1
			AND installation.installation_id = transaction.installation_id
		WHERE transaction.id = $1 AND transaction.poll_secret_hash = $2
			AND transaction.state = 'verified' AND transaction.expires_at > $3
		FOR UPDATE OF transaction
	`, id, pollSecretHash, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return ManagedLoginTransaction{}, ErrManagedLoginUnavailable
	}
	return transaction, err
}

func insertManagedLogoutContext(
	ctx context.Context,
	tx pgx.Tx,
	credentialID uuid.UUID,
	transaction ManagedLoginTransaction,
	sessionExpiresAt time.Time,
) error {
	if transaction.EncryptedLogoutToken == "" {
		return nil
	}
	expiresAt := transaction.LogoutExpiresAt
	if sessionExpiresAt.Before(expiresAt) {
		expiresAt = sessionExpiresAt
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_browser_logout_contexts (
			credential_id, encrypted_dek, encrypted_logout_token, expires_at
		) VALUES ($1, $2, $3, $4)
	`, credentialID, transaction.LogoutEncryptedDEK, transaction.EncryptedLogoutToken, expiresAt)
	return err
}

func resolveManagedIdentityUser(ctx context.Context, tx pgx.Tx, transaction ManagedLoginTransaction, at time.Time) (uuid.UUID, bool, error) {
	// Provider subject is durable after first binding; email is intentionally
	// consulted only for the initial invitation match.
	userID, status, provider, err := loadBoundManagedIdentity(ctx, tx, transaction.Issuer, transaction.ExternalSubject)
	if err == nil {
		if status != UserStatusActive || provider != transaction.Provider {
			return uuid.Nil, false, ErrManagedIdentityDenied
		}
		_, err = tx.Exec(ctx, `
			UPDATE fused_external_identities SET last_authenticated_at = $3
			WHERE issuer = $1 AND external_subject = $2
		`, transaction.Issuer, transaction.ExternalSubject, at)
		return userID, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	return bindInvitedManagedIdentity(ctx, tx, transaction, at)
}

func loadBoundManagedIdentity(ctx context.Context, tx pgx.Tx, issuer, externalSubject string) (uuid.UUID, UserStatus, string, error) {
	var userID uuid.UUID
	var status UserStatus
	var provider string
	err := tx.QueryRow(ctx, `
		SELECT binding.user_subject_id, subject.status, binding.provider
		FROM fused_external_identities binding
		JOIN fused_subjects subject ON subject.id = binding.user_subject_id
		WHERE binding.issuer = $1 AND binding.external_subject = $2
		FOR UPDATE OF binding, subject
	`, issuer, externalSubject).Scan(&userID, &status, &provider)
	return userID, status, provider, err
}

func bindInvitedManagedIdentity(ctx context.Context, tx pgx.Tx, transaction ManagedLoginTransaction, at time.Time) (uuid.UUID, bool, error) {
	normalizedEmail, _, err := normalizeUserEmail(transaction.VerifiedEmail)
	if err != nil {
		return uuid.Nil, false, ErrManagedIdentityDenied
	}
	var userID uuid.UUID
	// The unique normalized-email index and row lock make invitation selection
	// exact and atomic without loading candidate users into application memory.
	err = tx.QueryRow(ctx, `
		SELECT user_row.subject_id
		FROM fused_users user_row
		JOIN fused_subjects subject ON subject.id = user_row.subject_id
		LEFT JOIN fused_external_identities binding ON binding.user_subject_id = user_row.subject_id
		WHERE user_row.email_normalized = $1 AND subject.status = 'invited'
			AND binding.user_subject_id IS NULL
		FOR UPDATE OF user_row, subject
	`, normalizedEmail).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, ErrManagedIdentityDenied
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_external_identities (
			issuer, external_subject, provider, user_subject_id, last_authenticated_at
		) VALUES ($1, $2, $3, $4, $5)
	`, transaction.Issuer, transaction.ExternalSubject, transaction.Provider, userID, at)
	if err != nil {
		if isManagedIdentityConflict(err) {
			return uuid.Nil, false, ErrManagedIdentityDenied
		}
		return uuid.Nil, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE fused_subjects SET status = 'active', updated_at = $2 WHERE id = $1`, userID, at); err != nil {
		return uuid.Nil, false, err
	}
	return userID, true, nil
}

func isManagedIdentityConflict(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

func auditManagedLogin(ctx context.Context, tx pgx.Tx, transaction ManagedLoginTransaction, credential ControlCredential, revision int64, newBinding bool) error {
	command, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, resource_type, resource_id,
			trace_id, outcome, metadata
		)
		SELECT $1, $2::uuid, 'user.managed_login', 'workspace', workspace.id,
			$3, 'succeeded', jsonb_build_object(
				-- The credential parameter is also persisted as UUID above. Casting
				-- through UUID keeps PostgreSQL from inferring conflicting parameter types.
				'credential_id', ($2::uuid)::text, 'authorization_revision', $4::bigint,
				'provider', $5::text, 'auth_method', $6::text, 'new_binding', $7::boolean
			)
		FROM fused_workspaces workspace WHERE workspace.singleton_key = 1
	`, credential.UserID, credential.ID, accesscontrolTraceID(ctx), revision,
		transaction.Provider, transaction.AuthMethod, newBinding)
	if err != nil {
		return fmt.Errorf("audit managed login: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrManagedLoginUnavailable
	}
	return nil
}

func consumeManagedLoginRow(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time) error {
	// Identity claims have served their one purpose after binding. Clearing them
	// keeps transient provider PII out of long-lived Engine state.
	command, err := tx.Exec(ctx, `
		UPDATE fused_managed_login_transactions
		SET state = 'consumed', consumed_at = $2, provider = NULL, issuer = NULL,
			external_subject = NULL, verified_email = NULL, display_name = NULL,
			auth_method = NULL, authenticated_at = NULL, logout_encrypted_dek = NULL,
			encrypted_logout_token = NULL, logout_expires_at = NULL
		WHERE id = $1 AND state = 'verified'
	`, id, at)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrManagedLoginUnavailable
	}
	return nil
}

func (s *postgresStore) ExpireManagedLoginTransactions(ctx context.Context, at time.Time, limit int) (int64, error) {
	command, err := s.db.Exec(ctx, `
		WITH expired AS (
			SELECT id FROM fused_managed_login_transactions
			WHERE expires_at <= $1
			ORDER BY expires_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM fused_managed_login_transactions transaction
		USING expired WHERE transaction.id = expired.id
	`, at, limit)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

func (s *postgresStore) ExpireBrowserLogoutContexts(ctx context.Context, at time.Time, limit int) (int64, error) {
	command, err := s.db.Exec(ctx, `
		WITH expired AS (
			SELECT credential_id FROM fused_browser_logout_contexts
			WHERE expires_at <= $1
			ORDER BY expires_at, credential_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM fused_browser_logout_contexts logout
		USING expired WHERE logout.credential_id = expired.credential_id
	`, at, limit)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

func scanManagedLoginTransaction(row pgx.Row) (ManagedLoginTransaction, error) {
	var transaction ManagedLoginTransaction
	err := row.Scan(
		&transaction.ID, &transaction.RegistryTransactionID,
		&transaction.AccountID, &transaction.InstallationID, &transaction.Purpose,
		&transaction.PollSecretHash, &transaction.EnrollmentRef, &transaction.EncryptedDEK,
		&transaction.EncryptedRegistryVerifier, &transaction.State,
		&transaction.Provider, &transaction.Issuer, &transaction.ExternalSubject,
		&transaction.VerifiedEmail, &transaction.DisplayName, &transaction.AuthMethod,
		&transaction.AuthenticatedAt, &transaction.LogoutEncryptedDEK,
		&transaction.EncryptedLogoutToken, &transaction.LogoutExpiresAt, &transaction.ExpiresAt,
	)
	return transaction, err
}

func optionalManagedLogoutExpiry(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func accesscontrolTraceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

var _ ManagedIdentityStore = (*postgresStore)(nil)
