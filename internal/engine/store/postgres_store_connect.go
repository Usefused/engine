package store

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const connectConfigColumns = `
	id,  bucket_id, service_id, auth_type, auth_name, enabled,
	encrypted_dek, encrypted_client_id, encrypted_client_secret, redirect_uri,
	created_at, updated_at`

const authConnectionColumns = `
	id,  bucket_id, service_id, service_version_id, end_user_ref, created_by_app_id,
	auth_type, auth_name, encrypted_dek, access_token, refresh_token, id_token, token_type,
	scopes, scope_source, issuer, subject, identity_claims, expires_at, refresh_token_expires_at, last_used_at,
	last_refresh_attempt_at, last_refreshed_at, refresh_retry_not_before, refresh_state,
	last_failure_code, last_failure_at, last_failure_trace_id, created_at, updated_at`

const connectSessionColumns = `
	id,  bucket_id, service_id, service_version_id, auth_type, auth_name, end_user_ref, state_hash,
	nonce_hash, encrypted_dek, pkce_verifier, created_by_app_id, return_url, resource_input, requested_scopes, expires_at, used_at, created_at`

const connectInputSessionColumns = `
	id, bucket_id, service_id, auth_type, auth_name, contract_hash, end_user_ref, token_hash,
	created_by_app_id, return_url, resource_input, requested_scopes, expires_at, used_at, created_at`

func (s *postgresStore) UpsertConnectConfig(ctx context.Context, cfg ConnectConfig) (*ConnectConfig, error) {
	if err := validateConnectConfigMaterial(cfg); err != nil {
		return nil, err
	}
	query := `
		INSERT INTO fused_connect_configs (
			bucket_id, service_id, auth_type, auth_name, enabled,
			encrypted_dek, encrypted_client_id, encrypted_client_secret, redirect_uri
		)
		SELECT b.id, $2, $3, $4, $5, $6, $7, $8, $9
		FROM fused_buckets b
		WHERE b.id = $1
		ON CONFLICT ON CONSTRAINT uq_fused_connect_configs
		DO UPDATE SET
			auth_type = EXCLUDED.auth_type,
			auth_name = EXCLUDED.auth_name,
			enabled = EXCLUDED.enabled,
			encrypted_dek = EXCLUDED.encrypted_dek,
			encrypted_client_id = EXCLUDED.encrypted_client_id,
			encrypted_client_secret = EXCLUDED.encrypted_client_secret,
			redirect_uri = EXCLUDED.redirect_uri,
			updated_at = NOW()
		RETURNING ` + connectConfigColumns
	return scanConnectConfig(s.db.QueryRow(ctx, query,
		cfg.BucketID, cfg.ServiceID, cfg.AuthType, cfg.AuthName, cfg.Enabled,
		cfg.EncryptedDEK, cfg.EncryptedClientID, cfg.EncryptedClientSecret, cfg.RedirectURI,
	))
}

func (s *postgresStore) GetConnectConfig(ctx context.Context, bucketID, serviceID uuid.UUID) (*ConnectConfig, error) {
	query := `SELECT ` + connectConfigColumns + ` FROM fused_connect_configs WHERE bucket_id = $1 AND service_id = $2`
	cfg, err := scanConnectConfig(s.db.QueryRow(ctx, query, bucketID, serviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return cfg, err
}

func (s *postgresStore) ListConnectConfigsForBucket(ctx context.Context, bucketID uuid.UUID) ([]ConnectConfig, error) {
	query := `SELECT ` + connectConfigColumns + ` FROM fused_connect_configs WHERE bucket_id = $1 ORDER BY created_at DESC`
	rows, err := s.db.Query(ctx, query, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectConnectConfigs(rows)
}

func (s *postgresStore) ListConnectConfigsForService(ctx context.Context, serviceID uuid.UUID) ([]ConnectConfig, error) {
	query := `SELECT ` + connectConfigColumns + ` FROM fused_connect_configs
		WHERE service_id = $1
		ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.Query(ctx, query, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectConnectConfigs(rows)
}

// ListWorkspaceConnectConfigs returns every bucket-owned connect config in one
// SQL read. Service activation is intentionally not a filter here: buckets can
// hold credentials before an app chooses the service, and sync must not
// hide that material.
func (s *postgresStore) ListWorkspaceConnectConfigs(ctx context.Context) ([]WorkspaceConnectConfig, error) {
	query := `SELECT ` + prefixedConnectConfigColumns("configs") + `, buckets.name
		FROM fused_connect_configs configs
		JOIN fused_buckets buckets ON buckets.id = configs.bucket_id 
		
		ORDER BY configs.service_id, configs.updated_at DESC, configs.id DESC`
	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorkspaceConnectConfigs(rows)
}

// prefixedConnectConfigColumns keeps joined connect-config reads aligned with
// the canonical scanner while avoiding ambiguous column names in SQL joins.
func prefixedConnectConfigColumns(alias string) string {
	return prefixedColumns(connectConfigColumns, alias)
}

// prefixedAuthConnectionColumns qualifies the canonical credential projection
// used inside UPDATE CTEs where unqualified RETURNING names are ambiguous.
func prefixedAuthConnectionColumns(alias string) string {
	return prefixedColumns(authConnectionColumns, alias)
}

// prefixedColumns preserves a scanner's canonical order while qualifying every
// field for joined or data-modifying CTE projections.
func prefixedColumns(columnList, alias string) string {
	columns := strings.Split(strings.TrimSpace(columnList), ",")
	// Every selected column belongs to the supplied alias; qualifying at
	// construction time keeps each shared scanner order as the single contract.
	for index := range columns {
		columns[index] = alias + "." + strings.TrimSpace(columns[index])
	}
	return strings.Join(columns, ", ")
}

func (s *postgresStore) GetBucketConnectSummary(ctx context.Context, bucketID uuid.UUID) (*BucketConnectSummary, error) {
	query := `
		SELECT
			$1::uuid AS bucket_id,
			(SELECT COUNT(*) FROM fused_connect_configs WHERE bucket_id = $1) AS connect_config_count,
			(SELECT COUNT(DISTINCT end_user_ref) FROM fused_auth_connections WHERE bucket_id = $1) AS connected_user_count`
	var summary BucketConnectSummary
	err := s.db.QueryRow(ctx, query, bucketID).Scan(&summary.BucketID, &summary.ConnectConfigCount, &summary.ConnectedUserCount)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

// UpsertAuthConnection writes standalone credential refreshes through the same
// validated row helper used by callback transactions.
func (s *postgresStore) UpsertAuthConnection(ctx context.Context, conn AuthConnection) (*AuthConnection, error) {
	if err := validateAuthConnectionMaterial(conn); err != nil {
		return nil, err
	}
	return upsertAuthConnectionRow(ctx, s.db, conn)
}

type authConnectionRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// upsertAuthConnectionRow keeps the credential write identical for standalone
// refreshes and callback transactions while allowing the latter to share one
// PostgreSQL commit with resource reconciliation.
func upsertAuthConnectionRow(ctx context.Context, querier authConnectionRowQuerier, conn AuthConnection) (*AuthConnection, error) {
	query := `
		INSERT INTO fused_auth_connections (
			bucket_id, service_id, service_version_id, end_user_ref, created_by_app_id,
			auth_type, auth_name, encrypted_dek, access_token, refresh_token, id_token,
			token_type, scopes, scope_source, issuer, subject, identity_claims, expires_at, refresh_token_expires_at,
			refresh_state, last_failure_code, last_failure_at, last_failure_trace_id
		)
		SELECT b.id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17::jsonb, $18, $19,
		       $20, $21, $22, $23
		FROM fused_buckets b
		WHERE b.id = $1
		ON CONFLICT ON CONSTRAINT uq_fused_auth_connections
		DO UPDATE SET
			service_version_id = COALESCE(EXCLUDED.service_version_id, fused_auth_connections.service_version_id),
			created_by_app_id = EXCLUDED.created_by_app_id,
			auth_type = EXCLUDED.auth_type,
			auth_name = EXCLUDED.auth_name,
			encrypted_dek = EXCLUDED.encrypted_dek,
			access_token = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token,
			id_token = EXCLUDED.id_token,
			token_type = EXCLUDED.token_type,
			scopes = EXCLUDED.scopes,
			scope_source = EXCLUDED.scope_source,
			issuer = EXCLUDED.issuer,
			subject = EXCLUDED.subject,
			identity_claims = EXCLUDED.identity_claims,
			expires_at = EXCLUDED.expires_at,
			refresh_token_expires_at = EXCLUDED.refresh_token_expires_at,
			refresh_state = EXCLUDED.refresh_state,
			last_failure_code = EXCLUDED.last_failure_code,
			last_failure_at = EXCLUDED.last_failure_at,
			last_failure_trace_id = EXCLUDED.last_failure_trace_id,
			refresh_retry_not_before = NULL,
			refresh_lease_token = NULL,
			refresh_lease_expires_at = NULL,
			updated_at = NOW()
		RETURNING ` + authConnectionColumns
	return scanAuthConnection(querier.QueryRow(ctx, query,
		conn.BucketID, conn.ServiceID, uuidOrNil(conn.ServiceVersionID), conn.EndUserRef, uuidOrNil(conn.CreatedByAppID),
		conn.AuthType, conn.AuthName, conn.EncryptedDEK, conn.EncryptedAccessToken, emptyStringOrNil(conn.EncryptedRefreshToken), emptyStringOrNil(conn.EncryptedIDToken),
		defaultString(conn.TokenType, "Bearer"), nonNilStrings(conn.Scopes), defaultString(conn.ScopeSource, "none"), conn.Issuer, conn.Subject,
		jsonObjectBytes(conn.IdentityClaims), conn.ExpiresAt, conn.RefreshTokenExpiresAt, defaultString(conn.RefreshState, "ok"),
		conn.LastFailureCode, conn.LastFailureAt, conn.LastFailureTraceID,
	))
}

func (s *postgresStore) GetAuthConnection(ctx context.Context, bucketID, serviceID uuid.UUID, endUserRef, authName string) (*AuthConnection, error) {
	query := `SELECT ` + authConnectionColumns + ` FROM fused_auth_connections WHERE bucket_id = $1 AND service_id = $2 AND end_user_ref = $3 AND auth_name = $4`
	conn, err := scanAuthConnection(s.db.QueryRow(ctx, query, bucketID, serviceID, endUserRef, authName))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return conn, err
}

func (s *postgresStore) GetAuthConnectionByIDForBuckets(ctx context.Context, id uuid.UUID, bucketIDs []uuid.UUID) (*AuthConnection, error) {
	if len(bucketIDs) == 0 {
		return nil, nil
	}
	query := `SELECT ` + authConnectionColumns + ` FROM fused_auth_connections WHERE id = $1 AND bucket_id = ANY($2)`
	conn, err := scanAuthConnection(s.db.QueryRow(ctx, query, id, bucketIDs))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return conn, err
}

func (s *postgresStore) GetAuthConnectionsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AuthConnection, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]AuthConnection{}, nil
	}
	rows, err := s.db.Query(ctx, `SELECT `+authConnectionColumns+` FROM fused_auth_connections WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	connections := make(map[uuid.UUID]AuthConnection, len(ids))
	for rows.Next() {
		connection, err := scanAuthConnection(rows)
		if err != nil {
			return nil, err
		}
		connections[connection.ID] = *connection
	}
	return connections, rows.Err()
}

func (s *postgresStore) ListAuthConnections(ctx context.Context, bucketID uuid.UUID, serviceID *uuid.UUID, endUserRef string) ([]AuthConnection, error) {
	query := `
		SELECT ` + authConnectionColumns + `
		FROM fused_auth_connections
		WHERE bucket_id = $1
		AND ($2::uuid IS NULL OR service_id = $2)
		AND ($3 = '' OR end_user_ref = $3)
		ORDER BY updated_at DESC`
	rows, err := s.db.Query(ctx, query, bucketID, nullableUUIDPtr(serviceID), endUserRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectAuthConnections(rows)
}

func (s *postgresStore) ListAuthConnectionsPage(ctx context.Context, bucketID uuid.UUID, serviceID *uuid.UUID, endUserRef string, limit, offset int) ([]AuthConnection, int, error) {
	args := []any{bucketID, nullableUUIDPtr(serviceID), endUserRef}
	where := `WHERE bucket_id = $1 AND ($2::uuid IS NULL OR service_id = $2) AND ($3 = '' OR end_user_ref = $3)`
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_auth_connections `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT ` + authConnectionColumns + `
		FROM fused_auth_connections
		` + where + `
		ORDER BY updated_at DESC
		LIMIT $4 OFFSET $5`
	args = append(args, limit, offset)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	connections, err := collectAuthConnections(rows)
	return connections, total, err
}

func (s *postgresStore) DeleteAuthConnection(ctx context.Context, bucketID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_auth_connections WHERE bucket_id = $1 AND id = $2`, bucketID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAuthConnectionNotFound
	}
	return nil
}

func (s *postgresStore) TouchAuthConnectionLastUsed(ctx context.Context, id uuid.UUID, usedAt time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE fused_auth_connections SET last_used_at = $2, updated_at = NOW() WHERE id = $1`, id, usedAt)
	return err
}

// RecordAuthConnectionFailure updates only sanitized diagnostic metadata so a
// provider authorization failure can never overwrite encrypted token fields.
func (s *postgresStore) RecordAuthConnectionFailure(ctx context.Context, id uuid.UUID, code, traceID string, failedAt time.Time) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE fused_auth_connections
		SET last_failure_code = $2, last_failure_at = $3, last_failure_trace_id = $4, updated_at = NOW()
		WHERE id = $1`, id, code, failedAt, traceID)
	if err != nil {
		return err
	}
	// A missing row means the connection was removed while the provider request
	// was in flight; surfacing that race is more useful than pretending it logged.
	if tag.RowsAffected() == 0 {
		return ErrAuthConnectionNotFound
	}
	return nil
}

// ClaimAuthConnectionsForRefresh atomically leases one ordered worker page;
// SKIP LOCKED lets Engine replicas drain the same due set without blocking.
func (s *postgresStore) ClaimAuthConnectionsForRefresh(ctx context.Context, cutoff, passStartedAt, now, leaseExpiresAt time.Time, limit int) ([]AuthConnectionRefreshClaim, error) {
	if limit <= 0 || !leaseExpiresAt.After(now) {
		return nil, ErrInvalidAuthConnectionRefreshClaim
	}
	query := `WITH candidates AS (
		SELECT id
		FROM fused_auth_connections
		WHERE service_version_id IS NOT NULL
		  AND lower(auth_type) IN ('oauth', 'oauth2', 'oidc', 'openidconnect', 'open_id_connect')
		  AND refresh_state IN ('ok', 'failed', 'expired')
		  AND COALESCE(LEAST(expires_at, refresh_token_expires_at), expires_at, refresh_token_expires_at) <= $1
		  AND (refresh_token IS NOT NULL OR expires_at <= $3)
		  AND (refresh_retry_not_before IS NULL OR refresh_retry_not_before <= $3)
		  AND (refresh_lease_expires_at IS NULL OR refresh_lease_expires_at <= $3)
		  AND (last_refresh_attempt_at IS NULL OR last_refresh_attempt_at < $2)
		ORDER BY COALESCE(LEAST(expires_at, refresh_token_expires_at), expires_at, refresh_token_expires_at) ASC, id ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $5
	), claimed AS (
		UPDATE fused_auth_connections AS connection
		SET refresh_lease_token = gen_random_uuid(),
			refresh_lease_expires_at = $4,
			last_refresh_attempt_at = $3,
			updated_at = $3
		FROM candidates
		WHERE connection.id = candidates.id
		RETURNING ` + prefixedAuthConnectionColumns("connection") + `,
			connection.refresh_lease_token, connection.refresh_lease_expires_at
	)
	SELECT * FROM claimed
	ORDER BY COALESCE(LEAST(expires_at, refresh_token_expires_at), expires_at, refresh_token_expires_at) ASC, id ASC`
	rows, err := s.db.Query(ctx, query, cutoff, passStartedAt, now, leaseExpiresAt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectAuthConnectionRefreshClaims(rows)
}

// TryClaimAuthConnectionRefresh leases one foreground connection and binds a
// legacy nil version only to the exact version already resolved for execution.
func (s *postgresStore) TryClaimAuthConnectionRefresh(ctx context.Context, id, serviceVersionID uuid.UUID, now, leaseExpiresAt time.Time) (*AuthConnectionRefreshClaim, error) {
	if id == uuid.Nil || serviceVersionID == uuid.Nil || !leaseExpiresAt.After(now) {
		return nil, ErrInvalidAuthConnectionRefreshClaim
	}
	query := `UPDATE fused_auth_connections
		SET service_version_id = COALESCE(service_version_id, $2),
			refresh_lease_token = gen_random_uuid(),
			refresh_lease_expires_at = $4,
			last_refresh_attempt_at = $3,
			updated_at = $3
		WHERE id = $1
		  AND refresh_state IN ('ok', 'failed', 'expired')
		  AND COALESCE(LEAST(expires_at, refresh_token_expires_at), expires_at, refresh_token_expires_at) <= $3::timestamptz + INTERVAL '5 minutes'
		  AND (refresh_token IS NOT NULL OR expires_at <= $3)
		  AND (refresh_lease_expires_at IS NULL OR refresh_lease_expires_at <= $3)
		  AND (refresh_retry_not_before IS NULL OR refresh_retry_not_before <= $3)
		RETURNING ` + authConnectionColumns + `, refresh_lease_token, refresh_lease_expires_at`
	claim, err := scanAuthConnectionRefreshClaim(s.db.QueryRow(ctx, query, id, serviceVersionID, now, leaseExpiresAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return claim, err
}

// CompleteAuthConnectionRefresh stores rotated encrypted material only while
// the caller still owns the lease, rejecting stale cross-replica completions.
func (s *postgresStore) CompleteAuthConnectionRefresh(ctx context.Context, id, leaseToken uuid.UUID, refreshed AuthConnection, refreshedAt time.Time) (*AuthConnection, bool, error) {
	if leaseToken == uuid.Nil || validateAuthConnectionMaterial(refreshed) != nil {
		return nil, false, ErrInvalidEncryptedAuthMaterial
	}
	query := `UPDATE fused_auth_connections
		SET encrypted_dek = $3,
			access_token = $4,
			refresh_token = COALESCE($5, refresh_token),
			id_token = COALESCE($6, id_token),
			token_type = $7,
			scopes = $8,
			scope_source = $9,
			issuer = $10,
			subject = $11,
			identity_claims = $12::jsonb,
			expires_at = $13,
			refresh_token_expires_at = $14,
			last_refreshed_at = $15,
			refresh_retry_not_before = $16,
			refresh_state = 'ok',
			last_failure_code = '',
			last_failure_at = NULL,
			last_failure_trace_id = '',
			refresh_lease_token = NULL,
			refresh_lease_expires_at = NULL,
			updated_at = $15
		WHERE id = $1
		  AND refresh_lease_token = $2
		  AND refresh_lease_expires_at > $15
		RETURNING ` + authConnectionColumns
	connection, err := scanAuthConnection(s.db.QueryRow(ctx, query,
		id, leaseToken, refreshed.EncryptedDEK, refreshed.EncryptedAccessToken,
		emptyStringOrNil(refreshed.EncryptedRefreshToken), emptyStringOrNil(refreshed.EncryptedIDToken),
		defaultString(refreshed.TokenType, "Bearer"), nonNilStrings(refreshed.Scopes),
		defaultString(refreshed.ScopeSource, "none"), refreshed.Issuer, refreshed.Subject,
		jsonObjectBytes(refreshed.IdentityClaims), refreshed.ExpiresAt, refreshed.RefreshTokenExpiresAt, refreshedAt,
		refreshed.RefreshRetryNotBefore,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return connection, err == nil, err
}

// ReleaseAuthConnectionRefresh records a bounded transient failure and retry
// time while keeping refresh_state ok so a later worker pass can retry it.
func (s *postgresStore) ReleaseAuthConnectionRefresh(ctx context.Context, id, leaseToken uuid.UUID, retryNotBefore time.Time, failureCode, traceID string, failedAt time.Time) (bool, error) {
	return s.finishAuthConnectionRefreshLease(ctx, id, leaseToken, "ok", &retryNotBefore, failureCode, traceID, failedAt)
}

// MarkAuthConnectionReconnectRequired permanently stops managed refresh after
// the provider rejects the grant and exposes only sanitized failure metadata.
func (s *postgresStore) MarkAuthConnectionReconnectRequired(ctx context.Context, id, leaseToken uuid.UUID, failureCode, traceID string, failedAt time.Time) (bool, error) {
	return s.finishAuthConnectionRefreshLease(ctx, id, leaseToken, "reconnect_required", nil, failureCode, traceID, failedAt)
}

// finishAuthConnectionRefreshLease performs the shared lease-token CAS for
// transient release and permanent reconnect state transitions.
func (s *postgresStore) finishAuthConnectionRefreshLease(ctx context.Context, id, leaseToken uuid.UUID, state string, retryNotBefore *time.Time, failureCode, traceID string, failedAt time.Time) (bool, error) {
	if id == uuid.Nil || leaseToken == uuid.Nil {
		return false, ErrInvalidAuthConnectionRefreshClaim
	}
	tag, err := s.db.Exec(ctx, `UPDATE fused_auth_connections
		SET refresh_state = $3,
			refresh_retry_not_before = $4,
			last_failure_code = $5,
			last_failure_at = $6,
			last_failure_trace_id = $7,
			refresh_lease_token = NULL,
			refresh_lease_expires_at = NULL,
			updated_at = $6
		WHERE id = $1
		  AND refresh_lease_token = $2
		  AND refresh_lease_expires_at > $6`, id, leaseToken, state, retryNotBefore, failureCode, failedAt, traceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetAuthConnectionByID reloads one connection for internal lease contention;
// tenant-facing handlers must continue using bucket-authorized lookup methods.
func (s *postgresStore) GetAuthConnectionByID(ctx context.Context, id uuid.UUID) (*AuthConnection, error) {
	connection, err := scanAuthConnection(s.db.QueryRow(ctx, `SELECT `+authConnectionColumns+` FROM fused_auth_connections WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return connection, err
}

// CreateConnectSession uses the shared insert path so direct connect starts and
// form completions persist an identical provider callback record.
func (s *postgresStore) CreateConnectSession(ctx context.Context, session ConnectSession) (*ConnectSession, error) {
	return insertConnectSession(ctx, s.db, session)
}

type connectQueryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// insertConnectSession accepts either the pool or an existing transaction so
// form-token consumption can be atomic without duplicating insert SQL.
func insertConnectSession(ctx context.Context, db connectQueryRower, session ConnectSession) (*ConnectSession, error) {
	if session.ServiceVersionID == uuid.Nil || strings.TrimSpace(session.AuthType) == "" || strings.TrimSpace(session.AuthName) == "" {
		return nil, ErrInvalidEncryptedAuthMaterial
	}
	if session.EncryptedPKCEVerifier != "" && (!looksWrappedDEK(session.EncryptedDEK) || !looksEncryptedValue(session.EncryptedPKCEVerifier)) {
		return nil, ErrInvalidEncryptedAuthMaterial
	}
	query := `
		INSERT INTO fused_connect_sessions (
			bucket_id, service_id, service_version_id, auth_type, auth_name, end_user_ref, state_hash,
			nonce_hash, encrypted_dek, pkce_verifier, created_by_app_id, return_url, resource_input, requested_scopes, expires_at
			)
			SELECT b.id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
			FROM fused_buckets b
			WHERE b.id = $1
			RETURNING ` + connectSessionColumns
	return scanConnectSession(db.QueryRow(ctx, query,
		session.BucketID, session.ServiceID, session.ServiceVersionID, session.AuthType, session.AuthName, session.EndUserRef,
		session.StateHash, session.NonceHash, session.EncryptedDEK, session.EncryptedPKCEVerifier,
		uuidOrNil(session.CreatedByAppID), session.ReturnURL, jsonObjectBytes(session.ResourceInputJSON), session.RequestedScopes, session.ExpiresAt,
	))
}

func (s *postgresStore) GetConnectSessionByStateHash(ctx context.Context, stateHash string) (*ConnectSession, error) {
	query := `SELECT ` + connectSessionColumns + ` FROM fused_connect_sessions WHERE state_hash = $1`
	session, err := scanConnectSession(s.db.QueryRow(ctx, query, stateHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return session, err
}

func (s *postgresStore) MarkConnectSessionUsed(ctx context.Context, stateHash string, usedAt time.Time) error {
	tag, err := s.db.Exec(ctx, `UPDATE fused_connect_sessions SET used_at = $2 WHERE state_hash = $1 AND used_at IS NULL`, stateHash, usedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConnectSessionUnavailable
	}
	return nil
}

// CreateConnectInputSession stores the pre-authorisation browser handoff under
// a hash, keeping its raw bearer token exclusively in the returned URL.
func (s *postgresStore) CreateConnectInputSession(ctx context.Context, session ConnectInputSession) (*ConnectInputSession, error) {
	query := `
		INSERT INTO fused_connect_input_sessions (
			bucket_id, service_id, auth_type, auth_name, contract_hash, end_user_ref, token_hash,
			created_by_app_id, return_url, resource_input, requested_scopes, expires_at
		)
		SELECT b.id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
		FROM fused_buckets b
		WHERE b.id = $1
		RETURNING ` + connectInputSessionColumns
	return scanConnectInputSession(s.db.QueryRow(ctx, query,
		session.BucketID, session.ServiceID, session.AuthType, session.AuthName, session.ContractHash,
		session.EndUserRef, session.TokenHash, uuidOrNil(session.CreatedByAppID),
		session.ReturnURL, jsonObjectBytes(session.ResourceInputJSON), session.RequestedScopes, session.ExpiresAt,
	))
}

// GetActiveConnectInputSessionByTokenHash performs one exact indexed lookup
// with replay and expiry predicates in SQL, so inactive rows never enter Go.
func (s *postgresStore) GetActiveConnectInputSessionByTokenHash(ctx context.Context, tokenHash string) (*ConnectInputSession, error) {
	query := `SELECT ` + connectInputSessionColumns + ` FROM fused_connect_input_sessions WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()`
	session, err := scanConnectInputSession(s.db.QueryRow(ctx, query, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return session, err
}

// CompleteConnectInputSession consumes the browser form token and inserts the
// provider callback session in one transaction. A duplicate form submission
// cannot mint a second authorization request, while insertion failure leaves
// the input session retryable.
func (s *postgresStore) CompleteConnectInputSession(ctx context.Context, tokenHash, contractHash string, usedAt time.Time, session ConnectSession) (*ConnectSession, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Identity predicates bind the inserted callback session to the exact
	// pre-authorisation request without loading the pending row into Go. The
	// completed resource JSON is intentionally different because it now includes
	// the fields collected by the browser form.
	tag, err := tx.Exec(ctx, `
		UPDATE fused_connect_input_sessions
		SET used_at = $2
		WHERE token_hash = $1
		  AND used_at IS NULL
		  AND expires_at > $2
		  AND bucket_id = $3
		  AND service_id = $4
		  AND auth_type = $5
		  AND auth_name = $6
		  AND end_user_ref = $7
		  AND created_by_app_id IS NOT DISTINCT FROM $8
		  AND return_url = $9
		  AND requested_scopes = $10
		  AND contract_hash = $11`,
		tokenHash, usedAt, session.BucketID, session.ServiceID, session.AuthType,
		session.AuthName, session.EndUserRef, uuidOrNil(session.CreatedByAppID),
		session.ReturnURL, session.RequestedScopes, contractHash,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrConnectSessionUnavailable
	}
	created, err := insertConnectSession(ctx, tx, session)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// DeleteExpiredConnectSessions removes provider and pre-authorisation sessions
// in one bounded transaction so cleanup cannot leave either lifecycle behind.
func (s *postgresStore) DeleteExpiredConnectSessions(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	providerTag, err := tx.Exec(ctx, `DELETE FROM fused_connect_sessions WHERE expires_at < $1`, before)
	if err != nil {
		return 0, err
	}
	inputTag, err := tx.Exec(ctx, `DELETE FROM fused_connect_input_sessions WHERE expires_at < $1`, before)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return providerTag.RowsAffected() + inputTag.RowsAffected(), nil
}

type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanConnectConfig(row rowScanner) (*ConnectConfig, error) {
	var cfg ConnectConfig
	err := row.Scan(
		&cfg.ID, &cfg.BucketID, &cfg.ServiceID, &cfg.AuthType, &cfg.AuthName, &cfg.Enabled,
		&cfg.EncryptedDEK, &cfg.EncryptedClientID, &cfg.EncryptedClientSecret, &cfg.RedirectURI,
		&cfg.CreatedAt, &cfg.UpdatedAt,
	)
	return &cfg, err
}

func collectConnectConfigs(rows rowsScanner) ([]ConnectConfig, error) {
	var configs []ConnectConfig
	for rows.Next() {
		cfg, err := scanConnectConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, *cfg)
	}
	return configs, rows.Err()
}

// collectWorkspaceConnectConfigs scans the export projection without exposing
// encrypted fields outside the store layer.
func collectWorkspaceConnectConfigs(rows rowsScanner) ([]WorkspaceConnectConfig, error) {
	var configs []WorkspaceConnectConfig
	// Rows are already workspace- and activation-scoped by SQL, so this loop
	// only maps result rows and performs no application-side filtering.
	for rows.Next() {
		var config WorkspaceConnectConfig
		err := rows.Scan(
			&config.ID, &config.BucketID, &config.ServiceID,
			&config.AuthType, &config.AuthName, &config.Enabled, &config.EncryptedDEK,
			&config.EncryptedClientID, &config.EncryptedClientSecret,
			&config.RedirectURI, &config.CreatedAt, &config.UpdatedAt, &config.BucketName,
		)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

// scanAuthConnection reads the canonical credential projection without private
// lease suffix fields used by worker claims.
func scanAuthConnection(row rowScanner) (*AuthConnection, error) {
	return scanAuthConnectionWithSuffix(row)
}

// scanAuthConnectionWithSuffix scans the canonical connection projection plus
// optional private columns used only by refresh-claim persistence.
func scanAuthConnectionWithSuffix(row rowScanner, suffix ...any) (*AuthConnection, error) {
	var conn AuthConnection
	var createdBy, serviceVersionID *uuid.UUID
	var refreshToken, idToken *string
	targets := []any{
		&conn.ID, &conn.BucketID, &conn.ServiceID, &serviceVersionID, &conn.EndUserRef, &createdBy,
		&conn.AuthType, &conn.AuthName, &conn.EncryptedDEK, &conn.EncryptedAccessToken, &refreshToken, &idToken, &conn.TokenType,
		&conn.Scopes, &conn.ScopeSource, &conn.Issuer, &conn.Subject, &conn.IdentityClaims, &conn.ExpiresAt, &conn.RefreshTokenExpiresAt, &conn.LastUsedAt,
		&conn.LastRefreshAttemptAt, &conn.LastRefreshedAt, &conn.RefreshRetryNotBefore,
		&conn.RefreshState, &conn.LastFailureCode, &conn.LastFailureAt, &conn.LastFailureTraceID, &conn.CreatedAt, &conn.UpdatedAt,
	}
	err := row.Scan(append(targets, suffix...)...)
	if createdBy != nil {
		conn.CreatedByAppID = *createdBy
	}
	if serviceVersionID != nil {
		conn.ServiceVersionID = *serviceVersionID
	}
	if refreshToken != nil {
		conn.EncryptedRefreshToken = *refreshToken
	}
	if idToken != nil {
		conn.EncryptedIDToken = *idToken
	}
	return &conn, err
}

// scanAuthConnectionRefreshClaim keeps lease fields outside AuthConnection
// while reusing its exact canonical database projection.
func scanAuthConnectionRefreshClaim(row rowScanner) (*AuthConnectionRefreshClaim, error) {
	var claim AuthConnectionRefreshClaim
	connection, err := scanAuthConnectionWithSuffix(row, &claim.LeaseToken, &claim.LeaseExpiresAt)
	if err != nil {
		return nil, err
	}
	claim.Connection = *connection
	return &claim, nil
}

// collectAuthConnectionRefreshClaims scans one claimed worker page and reports
// any cursor error after all rows have been consumed.
func collectAuthConnectionRefreshClaims(rows rowsScanner) ([]AuthConnectionRefreshClaim, error) {
	var claims []AuthConnectionRefreshClaim
	for rows.Next() {
		claim, err := scanAuthConnectionRefreshClaim(rows)
		if err != nil {
			return nil, err
		}
		claims = append(claims, *claim)
	}
	return claims, rows.Err()
}

func collectAuthConnections(rows rowsScanner) ([]AuthConnection, error) {
	var connections []AuthConnection
	for rows.Next() {
		conn, err := scanAuthConnection(rows)
		if err != nil {
			return nil, err
		}
		connections = append(connections, *conn)
	}
	return connections, rows.Err()
}

// scanConnectSession tolerates legacy nil version IDs so callback code can fail
// those ambiguous sessions closed with a typed browser response.
func scanConnectSession(row rowScanner) (*ConnectSession, error) {
	var session ConnectSession
	var createdBy, serviceVersionID *uuid.UUID
	err := row.Scan(
		&session.ID, &session.BucketID, &session.ServiceID, &serviceVersionID, &session.AuthType, &session.AuthName, &session.EndUserRef,
		&session.StateHash, &session.NonceHash, &session.EncryptedDEK, &session.EncryptedPKCEVerifier, &createdBy,
		&session.ReturnURL, &session.ResourceInputJSON, &session.RequestedScopes, &session.ExpiresAt, &session.UsedAt, &session.CreatedAt,
	)
	if createdBy != nil {
		session.CreatedByAppID = *createdBy
	}
	if serviceVersionID != nil {
		session.ServiceVersionID = *serviceVersionID
	}
	return &session, err
}

// scanConnectInputSession centralizes the exact SQL projection used by create
// and lookup paths so column order cannot drift between browser-flow methods.
func scanConnectInputSession(row rowScanner) (*ConnectInputSession, error) {
	var session ConnectInputSession
	var createdBy *uuid.UUID
	err := row.Scan(
		&session.ID, &session.BucketID, &session.ServiceID, &session.AuthType, &session.AuthName, &session.ContractHash,
		&session.EndUserRef, &session.TokenHash, &createdBy, &session.ReturnURL,
		&session.ResourceInputJSON, &session.RequestedScopes, &session.ExpiresAt, &session.UsedAt, &session.CreatedAt,
	)
	if createdBy != nil {
		session.CreatedByAppID = *createdBy
	}
	return &session, err
}

func validateConnectConfigMaterial(cfg ConnectConfig) error {
	if strings.TrimSpace(cfg.AuthName) == "" || !looksWrappedDEK(cfg.EncryptedDEK) ||
		!looksEncryptedValue(cfg.EncryptedClientID) ||
		!looksEncryptedValue(cfg.EncryptedClientSecret) {
		return ErrInvalidEncryptedAuthMaterial
	}
	return nil
}

func validateAuthConnectionMaterial(conn AuthConnection) error {
	if strings.TrimSpace(conn.AuthName) == "" || !looksWrappedDEK(conn.EncryptedDEK) || !looksEncryptedValue(conn.EncryptedAccessToken) {
		return ErrInvalidEncryptedAuthMaterial
	}
	for _, value := range []string{conn.EncryptedRefreshToken, conn.EncryptedIDToken} {
		if value != "" && !looksEncryptedValue(value) {
			return ErrInvalidEncryptedAuthMaterial
		}
	}
	return nil
}

func looksWrappedDEK(value string) bool {
	return strings.HasPrefix(value, "v1:")
}

func looksEncryptedValue(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	return len(decoded) > 28
}

func uuidOrNil(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullableUUIDPtr(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return *id
}

func emptyStringOrNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func jsonObjectBytes(value []byte) []byte {
	if len(value) == 0 {
		return []byte(`{}`)
	}
	return value
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
