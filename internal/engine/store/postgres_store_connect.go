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
	id,  bucket_id, service_id, auth_type, enabled,
	encrypted_dek, encrypted_client_id, encrypted_client_secret, redirect_uri,
	created_at, updated_at`

const authConnectionColumns = `
	id,  bucket_id, service_id, end_user_ref, created_by_artifact_id,
	auth_type, encrypted_dek, access_token, refresh_token, id_token, token_type,
	scopes, scope_source, issuer, subject, identity_claims, expires_at, refresh_token_expires_at, last_used_at,
	refresh_state, last_failure_code, last_failure_at, last_failure_trace_id, created_at, updated_at`

const connectSessionColumns = `
	id,  bucket_id, service_id, end_user_ref, state_hash,
	nonce_hash, encrypted_dek, pkce_verifier, created_by_artifact_id, return_url, resource_input, requested_scopes, expires_at, used_at, created_at`

func (s *postgresStore) UpsertConnectConfig(ctx context.Context, cfg ConnectConfig) (*ConnectConfig, error) {
	if err := validateConnectConfigMaterial(cfg); err != nil {
		return nil, err
	}
	query := `
		INSERT INTO fused_connect_configs (
			bucket_id, service_id, auth_type, enabled,
			encrypted_dek, encrypted_client_id, encrypted_client_secret, redirect_uri
		)
		SELECT b.id, $2, $3, $4, $5, $6, $7, $8
		FROM fused_buckets b
		WHERE b.id = $1
		ON CONFLICT ON CONSTRAINT uq_fused_connect_configs
		DO UPDATE SET
			auth_type = EXCLUDED.auth_type,
			enabled = EXCLUDED.enabled,
			encrypted_dek = EXCLUDED.encrypted_dek,
			encrypted_client_id = EXCLUDED.encrypted_client_id,
			encrypted_client_secret = EXCLUDED.encrypted_client_secret,
			redirect_uri = EXCLUDED.redirect_uri,
			updated_at = NOW()
		RETURNING ` + connectConfigColumns
	return scanConnectConfig(s.db.QueryRow(ctx, query,
		cfg.BucketID, cfg.ServiceID, cfg.AuthType, cfg.Enabled,
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
// hold credentials before an artifact chooses the service, and sync must not
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
	columns := strings.Split(strings.TrimSpace(connectConfigColumns), ",")
	// Each selected column belongs to the connect-config alias; qualifying at
	// construction time keeps the shared column order as the single contract.
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

func (s *postgresStore) UpsertAuthConnection(ctx context.Context, conn AuthConnection) (*AuthConnection, error) {
	if err := validateAuthConnectionMaterial(conn); err != nil {
		return nil, err
	}
	query := `
		INSERT INTO fused_auth_connections (
			bucket_id, service_id, end_user_ref, created_by_artifact_id,
			auth_type, encrypted_dek, access_token, refresh_token, id_token,
			token_type, scopes, scope_source, issuer, subject, identity_claims, expires_at, refresh_token_expires_at,
			refresh_state, last_failure_code, last_failure_at, last_failure_trace_id
		)
		SELECT b.id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16, $17,
		       $18, $19, $20, $21
		FROM fused_buckets b
		WHERE b.id = $1
		ON CONFLICT ON CONSTRAINT uq_fused_auth_connections
		DO UPDATE SET
			created_by_artifact_id = EXCLUDED.created_by_artifact_id,
			auth_type = EXCLUDED.auth_type,
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
			updated_at = NOW()
		RETURNING ` + authConnectionColumns
	return scanAuthConnection(s.db.QueryRow(ctx, query,
		conn.BucketID, conn.ServiceID, conn.EndUserRef, uuidOrNil(conn.CreatedByArtifactID),
		conn.AuthType, conn.EncryptedDEK, conn.EncryptedAccessToken, emptyStringOrNil(conn.EncryptedRefreshToken), emptyStringOrNil(conn.EncryptedIDToken),
		defaultString(conn.TokenType, "Bearer"), nonNilStrings(conn.Scopes), defaultString(conn.ScopeSource, "none"), conn.Issuer, conn.Subject,
		jsonObjectBytes(conn.IdentityClaims), conn.ExpiresAt, conn.RefreshTokenExpiresAt, defaultString(conn.RefreshState, "ok"),
		conn.LastFailureCode, conn.LastFailureAt, conn.LastFailureTraceID,
	))
}

func (s *postgresStore) GetAuthConnection(ctx context.Context, bucketID, serviceID uuid.UUID, endUserRef string) (*AuthConnection, error) {
	query := `SELECT ` + authConnectionColumns + ` FROM fused_auth_connections WHERE bucket_id = $1 AND service_id = $2 AND end_user_ref = $3`
	conn, err := scanAuthConnection(s.db.QueryRow(ctx, query, bucketID, serviceID, endUserRef))
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

func (s *postgresStore) ListAuthConnectionsNeedingRefresh(ctx context.Context, cutoff time.Time, limit int) ([]AuthConnection, error) {
	query := `
		SELECT ` + authConnectionColumns + `
		FROM fused_auth_connections
		WHERE refresh_token IS NOT NULL
		AND refresh_state = 'ok'
		AND expires_at IS NOT NULL
		AND expires_at <= $1
		ORDER BY expires_at ASC
		LIMIT $2`
	rows, err := s.db.Query(ctx, query, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectAuthConnections(rows)
}

func (s *postgresStore) CreateConnectSession(ctx context.Context, session ConnectSession) (*ConnectSession, error) {
	if session.EncryptedPKCEVerifier != "" && (!looksWrappedDEK(session.EncryptedDEK) || !looksEncryptedValue(session.EncryptedPKCEVerifier)) {
		return nil, ErrInvalidEncryptedAuthMaterial
	}
	query := `
		INSERT INTO fused_connect_sessions (
			bucket_id, service_id, end_user_ref, state_hash,
			nonce_hash, encrypted_dek, pkce_verifier, created_by_artifact_id, return_url, resource_input, requested_scopes, expires_at
			)
			SELECT b.id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
			FROM fused_buckets b
			WHERE b.id = $1
			RETURNING ` + connectSessionColumns
	return scanConnectSession(s.db.QueryRow(ctx, query,
		session.BucketID, session.ServiceID, session.EndUserRef,
		session.StateHash, session.NonceHash, session.EncryptedDEK, session.EncryptedPKCEVerifier,
		uuidOrNil(session.CreatedByArtifactID), session.ReturnURL, jsonObjectBytes(session.ResourceInputJSON), session.RequestedScopes, session.ExpiresAt,
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

func (s *postgresStore) DeleteExpiredConnectSessions(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_connect_sessions WHERE expires_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanConnectConfig(row rowScanner) (*ConnectConfig, error) {
	var cfg ConnectConfig
	err := row.Scan(
		&cfg.ID, &cfg.BucketID, &cfg.ServiceID, &cfg.AuthType, &cfg.Enabled,
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
			&config.AuthType, &config.Enabled, &config.EncryptedDEK,
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

func scanAuthConnection(row rowScanner) (*AuthConnection, error) {
	var conn AuthConnection
	var createdBy *uuid.UUID
	var refreshToken, idToken *string
	err := row.Scan(
		&conn.ID, &conn.BucketID, &conn.ServiceID, &conn.EndUserRef, &createdBy,
		&conn.AuthType, &conn.EncryptedDEK, &conn.EncryptedAccessToken, &refreshToken, &idToken, &conn.TokenType,
		&conn.Scopes, &conn.ScopeSource, &conn.Issuer, &conn.Subject, &conn.IdentityClaims, &conn.ExpiresAt, &conn.RefreshTokenExpiresAt, &conn.LastUsedAt,
		&conn.RefreshState, &conn.LastFailureCode, &conn.LastFailureAt, &conn.LastFailureTraceID, &conn.CreatedAt, &conn.UpdatedAt,
	)
	if createdBy != nil {
		conn.CreatedByArtifactID = *createdBy
	}
	if refreshToken != nil {
		conn.EncryptedRefreshToken = *refreshToken
	}
	if idToken != nil {
		conn.EncryptedIDToken = *idToken
	}
	return &conn, err
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

func scanConnectSession(row rowScanner) (*ConnectSession, error) {
	var session ConnectSession
	var createdBy *uuid.UUID
	err := row.Scan(
		&session.ID, &session.BucketID, &session.ServiceID, &session.EndUserRef,
		&session.StateHash, &session.NonceHash, &session.EncryptedDEK, &session.EncryptedPKCEVerifier, &createdBy,
		&session.ReturnURL, &session.ResourceInputJSON, &session.RequestedScopes, &session.ExpiresAt, &session.UsedAt, &session.CreatedAt,
	)
	if createdBy != nil {
		session.CreatedByArtifactID = *createdBy
	}
	return &session, err
}

func validateConnectConfigMaterial(cfg ConnectConfig) error {
	if !looksWrappedDEK(cfg.EncryptedDEK) ||
		!looksEncryptedValue(cfg.EncryptedClientID) ||
		!looksEncryptedValue(cfg.EncryptedClientSecret) {
		return ErrInvalidEncryptedAuthMaterial
	}
	return nil
}

func validateAuthConnectionMaterial(conn AuthConnection) error {
	if !looksWrappedDEK(conn.EncryptedDEK) || !looksEncryptedValue(conn.EncryptedAccessToken) {
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
