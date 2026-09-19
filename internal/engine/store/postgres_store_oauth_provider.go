package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *postgresStore) CreateOAuthClient(ctx context.Context, input OAuthClientRegistration) (OAuthClientMutationResult, error) {
	if err := validateOAuthClientRegistration(input); err != nil {
		return OAuthClientMutationResult{}, err
	}
	tx, err := s.beginAccessMutation(ctx, input.Actor)
	if err != nil {
		return OAuthClientMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	client, err := createOAuthClientTx(ctx, tx, input)
	if err != nil {
		return OAuthClientMutationResult{}, err
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, false)
	if err != nil {
		return OAuthClientMutationResult{}, err
	}
	if err := auditOAuthClientMutation(ctx, tx, input.Actor, "oauth.client.create", client.ID, revision); err != nil {
		return OAuthClientMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OAuthClientMutationResult{}, fmt.Errorf("commit OAuth client creation: %w", err)
	}
	return OAuthClientMutationResult{Client: client, AuthorizationRevision: revision}, nil
}

// RegisterOAuthClient records anonymous registration without granting user access.
func (s *postgresStore) RegisterOAuthClient(ctx context.Context, input OAuthClientRegistration) (OAuthClientMutationResult, error) {
	// Reject invalid client metadata before opening a transaction.
	if err := validateOAuthClientRegistration(input); err != nil {
		return OAuthClientMutationResult{}, err
	}
	// Anonymous registration must never create permanent or overlong credentials.
	now := time.Now().UTC()
	if input.ExpiresAt == nil || !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(OAuthDynamicClientMaxTTL)) {
		return OAuthClientMutationResult{}, fmt.Errorf("%w: dynamic clients must expire within one hour", ErrInvalidOAuthClient)
	}

	// Registration precedes user login, so it cannot require a control credential.
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	// No client may be written unless its audit can share the transaction.
	if err != nil {
		return OAuthClientMutationResult{}, fmt.Errorf("begin OAuth client registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	client, err := createOAuthClientTx(ctx, tx, input)
	// Roll back the transaction when the client cannot be persisted.
	if err != nil {
		return OAuthClientMutationResult{}, err
	}
	// A client identity must never outlive a failed registration audit.
	if err := auditOAuthClientMutation(ctx, tx, input.Actor, "oauth.client.register", client.ID, 0); err != nil {
		return OAuthClientMutationResult{}, err
	}
	// Return credentials only after both client and audit are committed.
	if err := tx.Commit(ctx); err != nil {
		return OAuthClientMutationResult{}, fmt.Errorf("commit OAuth client registration: %w", err)
	}
	return OAuthClientMutationResult{Client: client}, nil
}

func createOAuthClientTx(ctx context.Context, tx pgx.Tx, input OAuthClientRegistration) (OAuthClient, error) {
	var client OAuthClient
	var secretHash *string
	if input.ClientSecretHash != "" {
		secretHash = &input.ClientSecretHash
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_oauth_clients (
			name, client_id, client_secret_hash, redirect_uris, allowed_scopes, client_type, created_by_subject_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, name, client_id, redirect_uris, allowed_scopes, client_type, (client_secret_hash IS NOT NULL), created_at, expires_at, revoked_at
	`, input.Name, input.ClientID, secretHash, nonNilStrings(input.RedirectURIs), nonNilStrings(input.AllowedScopes),
		input.ClientType, nullableUUID(input.Actor.SubjectID), input.ExpiresAt).Scan(
		&client.ID, &client.Name, &client.ClientID, &client.RedirectURIs, &client.AllowedScopes,
		&client.ClientType, &client.HasSecret, &client.CreatedAt, &client.ExpiresAt, &client.RevokedAt,
	)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("create OAuth client: %w", err)
	}
	return client, nil
}

func (s *postgresStore) ListOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, client_id, redirect_uris, allowed_scopes, client_type,
			(client_secret_hash IS NOT NULL), created_at, expires_at, revoked_at
		FROM fused_oauth_clients
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list OAuth clients: %w", err)
	}
	defer rows.Close()
	clients := make([]OAuthClient, 0)
	for rows.Next() {
		var client OAuthClient
		if err := rows.Scan(
			&client.ID, &client.Name, &client.ClientID, &client.RedirectURIs, &client.AllowedScopes,
			&client.ClientType, &client.HasSecret, &client.CreatedAt, &client.ExpiresAt, &client.RevokedAt,
		); err != nil {
			return nil, fmt.Errorf("scan OAuth client: %w", err)
		}
		clients = append(clients, client)
	}
	return clients, rows.Err()
}

// RevokeOAuthClient cascades to every live token issued under the client so a
// revoked (e.g. compromised) third-party integration loses access
// immediately, not just for new authorize/token requests.
func (s *postgresStore) RevokeOAuthClient(ctx context.Context, id uuid.UUID, actor MutationActor) (int64, error) {
	tx, err := s.beginAccessMutation(ctx, actor)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `UPDATE fused_oauth_clients SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return 0, fmt.Errorf("revoke OAuth client: %w", err)
	}
	if command.RowsAffected() != 1 {
		return 0, ErrOAuthClientNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fused_oauth_tokens SET revoked_at = NOW() WHERE client_id = $1 AND revoked_at IS NULL
	`, id); err != nil {
		return 0, fmt.Errorf("revoke OAuth client tokens: %w", err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return 0, err
	}
	if err := auditOAuthClientMutation(ctx, tx, actor, "oauth.client.revoke", id, revision); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit OAuth client revocation: %w", err)
	}
	return revision, nil
}

// auditOAuthClientMutation preserves attribution, using NULL before user login.
func auditOAuthClientMutation(ctx context.Context, tx pgx.Tx, actor MutationActor, action string, clientID uuid.UUID, revision int64) error {
	command, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, resource_type, resource_id,
			request_id, trace_id, outcome, metadata
		)
		SELECT $1, $2, $3, 'workspace', workspace.id, $4, $5, 'succeeded',
			jsonb_build_object('oauth_client_id', $6::text, 'authorization_revision', $7::bigint)
		FROM fused_workspaces workspace WHERE workspace.singleton_key = 1
	`, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID), action, actor.RequestID, actor.TraceID, clientID, revision)
	// Propagate audit failures so the caller rolls back the credential mutation.
	if err != nil {
		return fmt.Errorf("audit OAuth client mutation: %w", err)
	}
	// A registration without a workspace cannot have a valid audit record.
	if command.RowsAffected() != 1 {
		return ErrInvalidOAuthClient
	}
	return nil
}

func (s *postgresStore) GetOAuthClientByPublicID(ctx context.Context, clientID string) (OAuthClient, string, error) {
	var client OAuthClient
	var secretHash *string
	err := s.db.QueryRow(ctx, `
		SELECT id, name, client_id, redirect_uris, allowed_scopes, client_type,
			(client_secret_hash IS NOT NULL), created_at, expires_at, revoked_at, client_secret_hash
		FROM fused_oauth_clients
		WHERE client_id = $1 AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > NOW())
	`, clientID).Scan(
		&client.ID, &client.Name, &client.ClientID, &client.RedirectURIs, &client.AllowedScopes,
		&client.ClientType, &client.HasSecret, &client.CreatedAt, &client.ExpiresAt, &client.RevokedAt, &secretHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, "", ErrOAuthClientNotFound
	}
	if err != nil {
		return OAuthClient{}, "", fmt.Errorf("get OAuth client: %w", err)
	}
	hash := ""
	if secretHash != nil {
		hash = *secretHash
	}
	return client, hash, nil
}

func (s *postgresStore) GetOAuthUserConsent(ctx context.Context, clientID, subjectID uuid.UUID) (OAuthConsent, bool, error) {
	var consent OAuthConsent
	err := s.db.QueryRow(ctx, `
		SELECT client_id, subject_id, granted_scope, granted_at
		FROM fused_oauth_user_consents
		WHERE client_id = $1 AND subject_id = $2
	`, clientID, subjectID).Scan(&consent.ClientID, &consent.SubjectID, &consent.GrantedScope, &consent.GrantedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthConsent{}, false, nil
	}
	if err != nil {
		return OAuthConsent{}, false, fmt.Errorf("get OAuth user consent: %w", err)
	}
	return consent, true, nil
}

// RecordOAuthConsentAndIssueCode persists (or refreshes) the user's consent
// and mints the single-use authorization code in one transaction, so a code
// is never issued without a matching consent record on file.
func (s *postgresStore) RecordOAuthConsentAndIssueCode(ctx context.Context, consent OAuthConsentGrant, code OAuthAuthorizationCodeIssue) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin OAuth consent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize each user's grants so concurrent approvals cannot exceed the cap.
	if err := enforceOAuthDynamicClientLimit(ctx, tx, consent); err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO fused_oauth_user_consents (client_id, subject_id, granted_scope)
		VALUES ($1, $2, $3)
		ON CONFLICT (client_id, subject_id) DO UPDATE SET
			granted_scope = EXCLUDED.granted_scope, updated_at = NOW()
	`, consent.ClientID, consent.SubjectID, nonNilStrings(consent.GrantedScope))
	if err != nil {
		return fmt.Errorf("record OAuth consent: %w", err)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO fused_oauth_authorization_codes (
			id, client_id, subject_id, redirect_uri, scope, code_hash, code_challenge, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, code.ID, code.ClientID, code.SubjectID, code.RedirectURI, nonNilStrings(code.Scope),
		code.CodeHash, code.CodeChallenge, code.ExpiresAt)
	if err != nil {
		return fmt.Errorf("issue OAuth authorization code: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrOAuthGrantDenied
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit OAuth consent: %w", err)
	}
	return nil
}

// enforceOAuthDynamicClientLimit holds the user's row lock until consent commits.
// Counting consents excludes anonymous registrations and lets disconnect release a slot.
func enforceOAuthDynamicClientLimit(ctx context.Context, tx pgx.Tx, consent OAuthConsentGrant) error {
	var subjectID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM fused_subjects
		WHERE id = $1 AND status = 'active' FOR UPDATE
	`, consent.SubjectID).Scan(&subjectID)
	// Consent cannot create grants for missing or inactive users.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOAuthGrantDenied
	}
	// A failed lock must not fall back to an unprotected count.
	if err != nil {
		return fmt.Errorf("lock OAuth consent subject: %w", err)
	}
	var expiresAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT expires_at FROM fused_oauth_clients WHERE id = $1 AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > clock_timestamp())
	`, consent.ClientID).Scan(&expiresAt)
	// Re-check liveness after waiting for the user lock.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOAuthGrantDenied
	}
	// Storage errors cannot allow a grant to bypass its policy.
	if err != nil {
		return fmt.Errorf("check OAuth consent client: %w", err)
	}
	// Admin-managed integrations are outside the dynamic-client quota.
	if expiresAt == nil {
		return nil
	}
	var count int
	err = tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM fused_oauth_user_consents consent
		JOIN fused_oauth_clients client ON client.id = consent.client_id
		WHERE consent.subject_id = $1 AND consent.client_id <> $2
			AND client.revoked_at IS NULL AND client.expires_at > clock_timestamp()
	`, consent.SubjectID, consent.ClientID).Scan(&count)
	// Keep the quota closed if its authoritative count is unavailable.
	if err != nil {
		return fmt.Errorf("count active dynamic OAuth clients: %w", err)
	}
	// Reauthorizing the same client takes no extra slot; a new eleventh one is denied.
	if count >= MaxActiveOAuthDynamicClientsPerUser {
		return ErrOAuthDynamicClientLimit
	}
	return nil
}

type lockedOAuthCode struct {
	id            uuid.UUID
	clientID      uuid.UUID
	subjectID     uuid.UUID
	redirectURI   string
	scope         []string
	codeChallenge string
	expiresAt     time.Time
	consumedAt    *time.Time
}

func (s *postgresStore) ExchangeOAuthAuthorizationCode(ctx context.Context, input OAuthCodeExchange, at time.Time) (OAuthTokenMetadata, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return OAuthTokenMetadata{}, fmt.Errorf("begin OAuth code exchange: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	metadata, err := exchangeOAuthAuthorizationCodeTx(ctx, tx, input, at)
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OAuthTokenMetadata{}, fmt.Errorf("commit OAuth code exchange: %w", err)
	}
	return metadata, nil
}

func exchangeOAuthAuthorizationCodeTx(ctx context.Context, tx pgx.Tx, input OAuthCodeExchange, at time.Time) (OAuthTokenMetadata, error) {
	code, err := lockOAuthAuthorizationCode(ctx, tx, input.CodeHash)
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	if !validOAuthCodeForExchange(code, input, at) {
		return OAuthTokenMetadata{}, ErrOAuthGrantDenied
	}
	if err := markOAuthAuthorizationCodeConsumed(ctx, tx, code.id, at); err != nil {
		return OAuthTokenMetadata{}, err
	}
	metadata, err := insertOAuthTokenPair(ctx, tx, code.clientID, code.subjectID, uuid.New(), code.scope, input.Issue)
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	if err := auditOAuthTokenLifecycle(ctx, tx, code.clientID, code.subjectID, metadata.ID, "oauth.token.issue"); err != nil {
		return OAuthTokenMetadata{}, err
	}
	return metadata, nil
}

func lockOAuthAuthorizationCode(ctx context.Context, tx pgx.Tx, codeHash string) (lockedOAuthCode, error) {
	var code lockedOAuthCode
	err := tx.QueryRow(ctx, `
		SELECT code.id, code.client_id, code.subject_id, code.redirect_uri, code.scope,
			code.code_challenge, code.expires_at, code.consumed_at
		FROM fused_oauth_authorization_codes code
		JOIN fused_oauth_clients client ON client.id = code.client_id AND client.revoked_at IS NULL
		WHERE code.code_hash = $1
		FOR UPDATE OF code
	`, codeHash).Scan(
		&code.id, &code.clientID, &code.subjectID, &code.redirectURI, &code.scope,
		&code.codeChallenge, &code.expiresAt, &code.consumedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedOAuthCode{}, ErrOAuthGrantDenied
	}
	if err != nil {
		return lockedOAuthCode{}, fmt.Errorf("lock OAuth authorization code: %w", err)
	}
	return code, nil
}

func validOAuthCodeForExchange(code lockedOAuthCode, input OAuthCodeExchange, at time.Time) bool {
	if code.consumedAt != nil || !code.expiresAt.After(at) {
		return false
	}
	if code.clientID != input.ClientID || code.redirectURI != input.RedirectURI {
		return false
	}
	return validOAuthPKCEVerifier(code.codeChallenge, input.CodeVerifier)
}

// validOAuthPKCEVerifier applies the RFC 7636 S256 transform; only that
// method is ever stored (see chk_fused_oauth_authorization_codes_pkce).
func validOAuthPKCEVerifier(codeChallenge, verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	digest := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(digest[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(codeChallenge)) == 1
}

func markOAuthAuthorizationCodeConsumed(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time) error {
	command, err := tx.Exec(ctx, `
		UPDATE fused_oauth_authorization_codes SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL
	`, id, at)
	if err != nil {
		return fmt.Errorf("consume OAuth authorization code: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrOAuthGrantDenied
	}
	return nil
}

type lockedOAuthToken struct {
	id               uuid.UUID
	clientID         uuid.UUID
	subjectID        uuid.UUID
	tokenFamilyID    uuid.UUID
	scope            []string
	consumedAt       *time.Time
	revokedAt        *time.Time
	refreshExpiresAt time.Time
}

func (s *postgresStore) RotateOAuthRefreshToken(ctx context.Context, input OAuthRefreshExchange, at time.Time) (OAuthTokenMetadata, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return OAuthTokenMetadata{}, fmt.Errorf("begin OAuth refresh rotation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	metadata, err := rotateOAuthRefreshTokenTx(ctx, tx, input, at)
	if err != nil {
		// Reuse detection revokes the entire token family as a side effect
		// inside this same transaction. That revocation is the actual
		// security response to a suspected replay, so it must be committed
		// even though the requested grant itself is denied -- rolling it
		// back here would silently leave the stolen/duplicated tokens live.
		if errors.Is(err, ErrOAuthGrantReuseDetected) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return OAuthTokenMetadata{}, fmt.Errorf("commit OAuth reuse revocation: %w", commitErr)
			}
		}
		return OAuthTokenMetadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OAuthTokenMetadata{}, fmt.Errorf("commit OAuth refresh rotation: %w", err)
	}
	return metadata, nil
}

func rotateOAuthRefreshTokenTx(ctx context.Context, tx pgx.Tx, input OAuthRefreshExchange, at time.Time) (OAuthTokenMetadata, error) {
	token, err := lockOAuthTokenByRefreshHash(ctx, tx, input.RefreshTokenHash)
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	if token.revokedAt != nil {
		return OAuthTokenMetadata{}, ErrOAuthGrantDenied
	}
	if token.consumedAt != nil {
		// The same refresh token was already exchanged once. A second use is
		// either a stolen-token replay or a client bug; either way the entire
		// rotation family is cut off immediately, not just this one row.
		if err := revokeOAuthTokenFamily(ctx, tx, token.tokenFamilyID, "oauth.token.reuse_detected"); err != nil {
			return OAuthTokenMetadata{}, err
		}
		return OAuthTokenMetadata{}, ErrOAuthGrantReuseDetected
	}
	if token.clientID != input.ClientID || !token.refreshExpiresAt.After(at) {
		return OAuthTokenMetadata{}, ErrOAuthGrantDenied
	}
	if err := markOAuthTokenConsumed(ctx, tx, token.id, at); err != nil {
		return OAuthTokenMetadata{}, err
	}
	metadata, err := insertOAuthTokenPair(ctx, tx, token.clientID, token.subjectID, token.tokenFamilyID, token.scope, input.Issue)
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	if err := auditOAuthTokenLifecycle(ctx, tx, token.clientID, token.subjectID, metadata.ID, "oauth.token.refresh"); err != nil {
		return OAuthTokenMetadata{}, err
	}
	return metadata, nil
}

func lockOAuthTokenByRefreshHash(ctx context.Context, tx pgx.Tx, refreshTokenHash string) (lockedOAuthToken, error) {
	var token lockedOAuthToken
	err := tx.QueryRow(ctx, `
		SELECT id, client_id, subject_id, token_family_id, scope, consumed_at, revoked_at, refresh_expires_at
		FROM fused_oauth_tokens
		WHERE refresh_token_hash = $1
		FOR UPDATE
	`, refreshTokenHash).Scan(
		&token.id, &token.clientID, &token.subjectID, &token.tokenFamilyID, &token.scope,
		&token.consumedAt, &token.revokedAt, &token.refreshExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedOAuthToken{}, ErrOAuthGrantDenied
	}
	if err != nil {
		return lockedOAuthToken{}, fmt.Errorf("lock OAuth refresh token: %w", err)
	}
	return token, nil
}

func markOAuthTokenConsumed(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time) error {
	command, err := tx.Exec(ctx, `
		UPDATE fused_oauth_tokens SET consumed_at = $2 WHERE id = $1 AND consumed_at IS NULL
	`, id, at)
	if err != nil {
		return fmt.Errorf("consume OAuth refresh token: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrOAuthGrantDenied
	}
	return nil
}

// revokeOAuthTokenFamily cascade-revokes every live token sharing a rotation
// family and bumps the authorization revision so any already-cached actor
// holding one of its access tokens is invalidated immediately, not merely at
// natural expiry.
func revokeOAuthTokenFamily(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, auditAction string) error {
	command, err := tx.Exec(ctx, `
		UPDATE fused_oauth_tokens SET revoked_at = NOW()
		WHERE token_family_id = $1 AND revoked_at IS NULL
	`, familyID)
	if err != nil {
		return fmt.Errorf("revoke OAuth token family: %w", err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (action, resource_type, trace_id, outcome, metadata)
		SELECT $1, 'workspace', $2, 'succeeded',
			jsonb_build_object('token_family_id', $3::text, 'revoked_count', $4::int, 'authorization_revision', $5::bigint)
	`, auditAction, accesscontrolTraceID(ctx), familyID, command.RowsAffected(), revision)
	if err != nil {
		return fmt.Errorf("audit OAuth token family revocation: %w", err)
	}
	return nil
}

// insertOAuthTokenPair bounds both token lifetimes by the live client deadline.
func insertOAuthTokenPair(ctx context.Context, tx pgx.Tx, clientID, subjectID, tokenFamilyID uuid.UUID, scope []string, issue OAuthTokenIssue) (OAuthTokenMetadata, error) {
	var metadata OAuthTokenMetadata
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_oauth_tokens (
			client_id, subject_id, token_family_id, access_token_hash, refresh_token_hash,
			scope, access_expires_at, refresh_expires_at
		) SELECT $1, $2, $3, $4, $5, $6,
			LEAST($7::timestamptz, client.expires_at), LEAST($8::timestamptz, client.expires_at)
		FROM fused_oauth_clients client
		WHERE client.id = $1 AND client.revoked_at IS NULL
			AND (client.expires_at IS NULL OR client.expires_at > clock_timestamp())
		RETURNING id, client_id, subject_id, token_family_id, scope, access_expires_at, refresh_expires_at
	`, clientID, subjectID, tokenFamilyID, issue.AccessTokenHash, issue.RefreshTokenHash,
		nonNilStrings(scope), issue.AccessExpiresAt, issue.RefreshExpiresAt).Scan(
		&metadata.ID, &metadata.ClientID, &metadata.SubjectID, &metadata.TokenFamilyID,
		&metadata.Scope, &metadata.AccessExpiresAt, &metadata.RefreshExpiresAt,
	)
	// Expiry or revocation racing the exchange must not issue another token.
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthTokenMetadata{}, ErrOAuthGrantDenied
	}
	// The grant transaction rolls back if credential persistence fails.
	if err != nil {
		return OAuthTokenMetadata{}, fmt.Errorf("issue OAuth token pair: %w", err)
	}
	return metadata, nil
}

func auditOAuthTokenLifecycle(ctx context.Context, tx pgx.Tx, clientID, subjectID, tokenID uuid.UUID, action string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (actor_subject_id, action, resource_type, trace_id, outcome, metadata)
		VALUES ($1, $2, 'app', $3, 'succeeded', jsonb_build_object('oauth_client_id', $4::text, 'oauth_token_id', $5::text))
	`, subjectID, action, accesscontrolTraceID(ctx), clientID, tokenID)
	if err != nil {
		return fmt.Errorf("audit OAuth token lifecycle: %w", err)
	}
	return nil
}

// ListOAuthConnectedApps hides expired clients immediately so their quota slots appear free.
func (s *postgresStore) ListOAuthConnectedApps(ctx context.Context, subjectID uuid.UUID) ([]OAuthConnectedApp, error) {
	rows, err := s.db.Query(ctx, `
		SELECT consent.client_id, client.name, consent.granted_scope, consent.granted_at
		FROM fused_oauth_user_consents consent
		JOIN fused_oauth_clients client ON client.id = consent.client_id AND client.revoked_at IS NULL
		WHERE consent.subject_id = $1 AND (client.expires_at IS NULL OR client.expires_at > NOW())
		ORDER BY consent.granted_at DESC
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("list OAuth connected apps: %w", err)
	}
	defer rows.Close()
	apps := make([]OAuthConnectedApp, 0)
	for rows.Next() {
		var app OAuthConnectedApp
		if err := rows.Scan(&app.ClientID, &app.ClientName, &app.GrantedScope, &app.GrantedAt); err != nil {
			return nil, fmt.Errorf("scan OAuth connected app: %w", err)
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// RevokeOAuthConnectedApp is the end-user "disconnect app" action: it removes
// the consent record and revokes every live token the app was ever issued for
// this subject, so a subsequent authorize request needs consent again and any
// currently-held access token stops working immediately.
func (s *postgresStore) RevokeOAuthConnectedApp(ctx context.Context, clientID, subjectID uuid.UUID, actor MutationActor) (int64, error) {
	tx, err := s.beginAccessMutation(ctx, actor)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `
		DELETE FROM fused_oauth_user_consents WHERE client_id = $1 AND subject_id = $2
	`, clientID, subjectID)
	if err != nil {
		return 0, fmt.Errorf("revoke OAuth consent: %w", err)
	}
	if command.RowsAffected() != 1 {
		return 0, ErrOAuthConsentNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fused_oauth_tokens SET revoked_at = NOW()
		WHERE client_id = $1 AND subject_id = $2 AND revoked_at IS NULL
	`, clientID, subjectID); err != nil {
		return 0, fmt.Errorf("revoke OAuth connected app tokens: %w", err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, resource_type, resource_id,
			request_id, trace_id, outcome, metadata
		) VALUES ($1, $2, 'oauth.consent.revoke', 'app', $3, $4, $5, 'succeeded',
			jsonb_build_object('authorization_revision', $6::bigint))
	`, actor.SubjectID, actor.CredentialID, clientID, actor.RequestID, actor.TraceID, revision)
	if err != nil {
		return 0, fmt.Errorf("audit OAuth connected app revocation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit OAuth connected app revocation: %w", err)
	}
	return revision, nil
}

// ExpireOAuthArtifacts deletes credential-bearing rows in bounded batches,
// mirroring ExpireAppTokens: the database selects expired rows and Go never
// loads a broad set merely to filter it by time.
// RevokeOAuthTokenByHash resolves tokenHash against either the access or
// refresh token column, scoped to the presenting client, and cascade-revokes
// its whole rotation family. It intentionally does not distinguish "not
// found" from "found but belongs to another client" in its return value:
// RFC 7009 requires the endpoint to behave identically either way.
func (s *postgresStore) RevokeOAuthTokenByHash(ctx context.Context, clientID uuid.UUID, tokenHash string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin OAuth token revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var familyID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT token_family_id FROM fused_oauth_tokens
		WHERE client_id = $1 AND (access_token_hash = $2 OR refresh_token_hash = $2) AND revoked_at IS NULL
		FOR UPDATE
	`, clientID, tokenHash).Scan(&familyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find OAuth token to revoke: %w", err)
	}
	if err := revokeOAuthTokenFamily(ctx, tx, familyID, "oauth.token.revoke"); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit OAuth token revocation: %w", err)
	}
	return true, nil
}

func (s *postgresStore) ExpireOAuthArtifacts(ctx context.Context, at time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, errors.New("OAuth artifact expiry limit must be positive")
	}
	var expiredCodes, expiredTokens, expiredClients int
	err := s.db.QueryRow(ctx, `
		WITH due_codes AS (
			SELECT id FROM fused_oauth_authorization_codes
			WHERE expires_at <= $1 ORDER BY expires_at, id
			LIMIT $2 FOR UPDATE SKIP LOCKED
		), deleted_codes AS (
			DELETE FROM fused_oauth_authorization_codes code USING due_codes
			WHERE code.id = due_codes.id RETURNING code.id
		), due_tokens AS (
			SELECT id FROM fused_oauth_tokens
			WHERE refresh_expires_at <= $1 ORDER BY refresh_expires_at, id
			LIMIT $2 FOR UPDATE SKIP LOCKED
		), deleted_tokens AS (
			DELETE FROM fused_oauth_tokens token USING due_tokens
			WHERE token.id = due_tokens.id RETURNING token.id
		), due_clients AS (
			SELECT id FROM fused_oauth_clients
			WHERE expires_at <= $1 AND revoked_at IS NULL
			ORDER BY expires_at, id LIMIT $2 FOR UPDATE SKIP LOCKED
		), revoked_clients AS (
			UPDATE fused_oauth_clients client SET revoked_at = NOW()
			FROM due_clients WHERE client.id = due_clients.id RETURNING client.id
		), revoked_client_tokens AS (
			UPDATE fused_oauth_tokens token SET revoked_at = NOW()
			FROM due_clients WHERE token.client_id = due_clients.id AND token.revoked_at IS NULL
		)
		SELECT (SELECT COUNT(*) FROM deleted_codes),
			(SELECT COUNT(*) FROM deleted_tokens),
			(SELECT COUNT(*) FROM revoked_clients)
	`, at, limit).Scan(&expiredCodes, &expiredTokens, &expiredClients)
	if err != nil {
		return 0, fmt.Errorf("expire OAuth artifacts: %w", err)
	}
	return expiredCodes + expiredTokens + expiredClients, nil
}

var _ OAuthClientStore = (*postgresStore)(nil)
