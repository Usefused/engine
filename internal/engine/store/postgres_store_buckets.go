package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
)

func (s *postgresStore) CreateBucket(ctx context.Context, name string, isDefault bool) (*Bucket, error) {
	query := `
		INSERT INTO fused_buckets (name, is_default)
		VALUES ($1, $2)
		RETURNING id, name, is_default, created_at, updated_at
	`
	var b Bucket
	err := s.db.QueryRow(ctx, query, name, isDefault).Scan(
		&b.ID, &b.Name, &b.IsDefault, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *postgresStore) ListBuckets(ctx context.Context) ([]Bucket, error) {
	query := `
		SELECT id, name, is_default, created_at, updated_at
		FROM fused_buckets
		ORDER BY created_at ASC
	`
	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.Name, &b.IsDefault, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// GetBucketsByNames resolves only plan-referenced buckets in one query so
// workspace planning neither scans all buckets nor performs N lookups.
func (s *postgresStore) GetBucketsByNames(ctx context.Context, names []string) ([]Bucket, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, name, is_default, created_at, updated_at
		FROM fused_buckets
		WHERE name = ANY($1)
		ORDER BY name`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0, len(names))
	for rows.Next() {
		var bucket Bucket
		if err := rows.Scan(&bucket.ID, &bucket.Name, &bucket.IsDefault, &bucket.CreatedAt, &bucket.UpdatedAt); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func (s *postgresStore) ListBucketSummaries(ctx context.Context, limit, offset int) ([]BucketSummary, int, error) {
	return s.ListAuthorizedBucketSummaries(ctx, accesscontrol.AuthorizedScope{All: true}, limit, offset)
}

func (s *postgresStore) ListAuthorizedBucketSummaries(ctx context.Context, scope accesscontrol.AuthorizedScope, limit, offset int) ([]BucketSummary, int, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_buckets WHERE $1 OR id = ANY($2::uuid[])`, scope.All, scope.IDs).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	query := `
		SELECT b.id, b.name, b.is_default, b.created_at, b.updated_at,
		       COALESCE(secret_counts.secret_count, 0), COALESCE(value_counts.value_count, 0),
		       COALESCE(user_counts.user_count, 0)
		FROM fused_buckets b
		LEFT JOIN (
			SELECT bucket_id, COUNT(*) AS secret_count
			FROM fused_workspace_secrets
			GROUP BY bucket_id
		) secret_counts ON secret_counts.bucket_id = b.id
		LEFT JOIN (
			SELECT bucket_id, COUNT(*) AS value_count
			FROM fused_bucket_values
			GROUP BY bucket_id
		) value_counts ON value_counts.bucket_id = b.id
		LEFT JOIN (
			SELECT bucket_id, COUNT(DISTINCT end_user_ref) AS user_count
			FROM fused_auth_connections
			GROUP BY bucket_id
		) user_counts ON user_counts.bucket_id = b.id
		WHERE $1 OR b.id = ANY($2::uuid[])
		ORDER BY b.is_default DESC, b.updated_at DESC, b.created_at DESC
		LIMIT $3 OFFSET $4
	`
	rows, err := s.db.Query(ctx, query, scope.All, scope.IDs, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var summaries []BucketSummary
	for rows.Next() {
		var summary BucketSummary
		if err := rows.Scan(
			&summary.ID, &summary.Name, &summary.IsDefault,
			&summary.CreatedAt, &summary.UpdatedAt, &summary.SecretCount, &summary.ValueCount,
			&summary.ConnectedUserCount,
		); err != nil {
			return nil, 0, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, total, rows.Err()
}

func (s *postgresStore) GetBucketSummary(ctx context.Context, bucketID uuid.UUID) (*BucketSummary, error) {
	query := `
		SELECT b.id, b.name, b.is_default, b.created_at, b.updated_at,
		       COALESCE(secret_counts.secret_count, 0), COALESCE(value_counts.value_count, 0),
		       COALESCE(user_counts.user_count, 0)
		FROM fused_buckets b
		LEFT JOIN (
			SELECT bucket_id, COUNT(*) AS secret_count
			FROM fused_workspace_secrets
			WHERE bucket_id = $1
			GROUP BY bucket_id
		) secret_counts ON secret_counts.bucket_id = b.id
		LEFT JOIN (
			SELECT bucket_id, COUNT(*) AS value_count
			FROM fused_bucket_values
			WHERE bucket_id = $1
			GROUP BY bucket_id
		) value_counts ON value_counts.bucket_id = b.id
		LEFT JOIN (
			SELECT bucket_id, COUNT(DISTINCT end_user_ref) AS user_count
			FROM fused_auth_connections
			WHERE bucket_id = $1
			GROUP BY bucket_id
		) user_counts ON user_counts.bucket_id = b.id
		WHERE b.id = $1
	`
	var summary BucketSummary
	err := s.db.QueryRow(ctx, query, bucketID).Scan(
		&summary.ID, &summary.Name, &summary.IsDefault,
		&summary.CreatedAt, &summary.UpdatedAt, &summary.SecretCount, &summary.ValueCount,
		&summary.ConnectedUserCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

func (s *postgresStore) GetBucket(ctx context.Context, bucketID uuid.UUID) (*Bucket, error) {
	query := `
		SELECT id, name, is_default, created_at, updated_at
		FROM fused_buckets
		WHERE id = $1
	`
	var b Bucket
	err := s.db.QueryRow(ctx, query, bucketID).Scan(
		&b.ID, &b.Name, &b.IsDefault, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *postgresStore) GetBucketByName(ctx context.Context, name string) (*Bucket, error) {
	query := `
		SELECT id, name, is_default, created_at, updated_at
		FROM fused_buckets
		WHERE name = $1
	`
	var b Bucket
	err := s.db.QueryRow(ctx, query, name).Scan(
		&b.ID, &b.Name, &b.IsDefault, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *postgresStore) DeleteBucket(ctx context.Context, name string, authorizedBucketID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.bucket.delete")
	defer span.End()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete bucket: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return err
	}
	bucketID, err := deleteBucketTx(ctx, tx, name, authorizedBucketID)
	if err != nil {
		return err
	}
	span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete bucket: commit: %w", err)
	}
	return nil
}

func deleteBucketTx(ctx context.Context, tx pgx.Tx, name string, authorizedBucketID uuid.UUID) (uuid.UUID, error) {
	bucketID, err := lockAuthorizedBucketForDelete(ctx, tx, name, authorizedBucketID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := ensureBucketUnbound(ctx, tx, bucketID); err != nil {
		return uuid.Nil, err
	}
	bindingTag, err := tx.Exec(ctx, `DELETE FROM fused_role_bindings WHERE resource_type = 'bucket' AND resource_id = $1`, bucketID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("delete bucket: remove role bindings: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM fused_buckets WHERE id = $1`, bucketID); err != nil {
		return uuid.Nil, fmt.Errorf("delete bucket: remove bucket: %w", err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, bindingTag.RowsAffected() > 0)
	if err != nil {
		return uuid.Nil, err
	}
	if err := auditBucketDelete(ctx, tx, bucketID, revision); err != nil {
		return uuid.Nil, err
	}
	return bucketID, nil
}

func lockAuthorizedBucketForDelete(ctx context.Context, tx pgx.Tx, name string, authorizedBucketID uuid.UUID) (uuid.UUID, error) {
	var bucketID uuid.UUID
	var isDefault bool
	err := tx.QueryRow(ctx, `SELECT id, is_default FROM fused_buckets WHERE name = $1 FOR UPDATE`, name).Scan(&bucketID, &isDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrBucketNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("delete bucket: lock: %w", err)
	}
	// The route authorized this immutable ID. A same-name replacement must not
	// inherit that decision while the request is in flight.
	if bucketID != authorizedBucketID {
		return uuid.Nil, ErrBucketNotFound
	}
	if isDefault {
		return uuid.Nil, ErrDefaultBucketProtected
	}
	return bucketID, nil
}

func ensureBucketUnbound(ctx context.Context, tx pgx.Tx, bucketID uuid.UUID) error {
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM fused_artifact_buckets WHERE bucket_id = $1)
		OR EXISTS (SELECT 1 FROM fused_workspace_webhooks WHERE secret_bucket_id = $1)`, bucketID).Scan(&bound); err != nil {
		return fmt.Errorf("delete bucket: inspect artifact bindings: %w", err)
	}
	if bound {
		return ErrBucketBound
	}
	return nil
}

func auditBucketDelete(ctx context.Context, tx pgx.Tx, bucketID uuid.UUID, revision int64) error {
	actor, _ := accesscontrol.ActorFromContext(ctx)
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (actor_subject_id, actor_credential_id, action, permission,
			resource_type, resource_id, trace_id, outcome, metadata)
		VALUES ($1, $2, 'bucket.delete', $3, 'bucket', $4, $5, 'succeeded',
			jsonb_build_object('authorization_revision', $6::bigint, 'changed', true))
	`, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID), accesscontrol.PermissionBucketManage,
		bucketID, trace.SpanFromContext(ctx).SpanContext().TraceID().String(), revision)
	if err != nil {
		return fmt.Errorf("audit bucket delete: %w", err)
	}
	return nil
}

// UpsertBucketValue stores an independent bucket literal value.
//
// This used to also "promote" the value into fused_bucket_bindings (a
// bucket-owned compiled-binding row) so runtime dispatch would pick it up
// alongside profile-generated bindings. fused_bucket_bindings was dropped by
// the workspace-scoped connection-profile migration -- see
// plans/workspace_connection_profile_scope_plan.md, which is explicit that
// "existing independent bucket values remain in fused_bucket_values; they
// are not migrated into connection profiles." The promotion side effect is
// therefore removed rather than redirected at a table that no longer
// exists; fused_bucket_values itself and its CRUD semantics are unchanged.
func (s *postgresStore) UpsertBucketValue(ctx context.Context, val BucketValue) error {
	if err := connectionprofile.ValidateLiteralBucketValue(val.Location, val.KeyName, val.Value); err != nil {
		return err
	}
	query := `
		INSERT INTO fused_bucket_values
		(bucket_id, service_id, key_name, location, value)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT ON CONSTRAINT uq_bucket_values
		DO UPDATE SET
			location = EXCLUDED.location,
			value = EXCLUDED.value,
			updated_at = NOW()
	`
	_, err := s.db.Exec(ctx, query, val.BucketID, val.ServiceID, val.KeyName, val.Location, val.Value)
	return err
}

func (s *postgresStore) GetBucketValues(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]BucketValue, error) {
	keyNames = uniqueSecretKeyNames(keyNames)
	if len(keyNames) == 0 {
		return nil, nil
	}
	query := `
		SELECT id, bucket_id, service_id, key_name, location, value, created_at, updated_at
		FROM fused_bucket_values
		WHERE bucket_id = $1 AND service_id = $2 AND key_name = ANY($3)
	`
	return s.collectBucketValues(ctx, query, bucketID, serviceID, keyNames)
}

func (s *postgresStore) ListBucketValues(ctx context.Context, bucketID uuid.UUID) ([]BucketValue, error) {
	query := `
		SELECT id, bucket_id, service_id, key_name, location, value, created_at, updated_at
		FROM fused_bucket_values
		WHERE bucket_id = $1
	`
	rows, err := s.db.Query(ctx, query, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []BucketValue
	for rows.Next() {
		var v BucketValue
		if err := rows.Scan(
			&v.ID, &v.BucketID, &v.ServiceID, &v.KeyName, &v.Location, &v.Value,
			&v.CreatedAt, &v.UpdatedAt,
		); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s *postgresStore) ListBucketValuePage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]BucketValue, int, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_bucket_values WHERE bucket_id = $1`, bucketID).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT id, bucket_id, service_id, key_name, location, value, created_at, updated_at
		FROM fused_bucket_values
		WHERE bucket_id = $1
		ORDER BY updated_at DESC, key_name ASC
		LIMIT $2 OFFSET $3
	`
	values, err := s.collectBucketValues(ctx, query, bucketID, limit, offset)
	return values, total, err
}

func (s *postgresStore) collectBucketValues(ctx context.Context, query string, args ...any) ([]BucketValue, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []BucketValue
	for rows.Next() {
		var v BucketValue
		if err := rows.Scan(
			&v.ID, &v.BucketID, &v.ServiceID, &v.KeyName, &v.Location, &v.Value,
			&v.CreatedAt, &v.UpdatedAt,
		); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s *postgresStore) ListBucketValuesForBuckets(ctx context.Context, bucketIDs []uuid.UUID) ([]BucketValue, error) {
	if len(bucketIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT id, bucket_id, key_name, value, created_at, updated_at
		FROM fused_bucket_values
		WHERE bucket_id = ANY($1)
	`
	rows, err := s.db.Query(ctx, query, bucketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []BucketValue
	for rows.Next() {
		var val BucketValue
		if err := rows.Scan(
			&val.ID, &val.BucketID, &val.KeyName, &val.Value,
			&val.CreatedAt, &val.UpdatedAt,
		); err != nil {
			return nil, err
		}
		values = append(values, val)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return values, nil
}

// DeleteBucketValue removes an independent bucket literal value. See
// UpsertBucketValue's comment: the matching fused_bucket_bindings cleanup
// this used to perform is gone along with that table.
func (s *postgresStore) DeleteBucketValue(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	query := `
		DELETE FROM fused_bucket_values
		WHERE bucket_id = $1
		AND service_id = $2
		AND key_name = $3
	`
	_, err := s.db.Exec(ctx, query, bucketID, serviceID, keyName)
	return err
}

func (s *postgresStore) ListBucketsForSDK(ctx context.Context, artifactID uuid.UUID) ([]Bucket, error) {
	return s.ListAuthorizedBucketsForSDK(ctx, artifactID, accesscontrol.AuthorizedScope{All: true})
}

func (s *postgresStore) ListAuthorizedBucketsForSDK(ctx context.Context, artifactID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]Bucket, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT b.id, b.name, b.is_default, b.created_at, b.updated_at
		FROM fused_buckets b
		JOIN fused_artifact_buckets sb ON b.id = sb.bucket_id
		WHERE sb.artifact_id = $1 AND ($2 OR b.id = ANY($3::uuid[]))
		ORDER BY b.created_at ASC
	`
	rows, err := s.db.Query(ctx, query, artifactID, scope.All, scope.IDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.Name, &b.IsDefault, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

func (s *postgresStore) ListArtifactScopesForBucket(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]ArtifactScope, int, error) {
	return s.ListAuthorizedArtifactScopesForBucket(ctx, bucketID, accesscontrol.AuthorizedScope{All: true}, limit, offset)
}

func (s *postgresStore) ListAuthorizedArtifactScopesForBucket(ctx context.Context, bucketID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]ArtifactScope, int, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_artifact_buckets WHERE bucket_id = $1 AND ($2 OR artifact_id = ANY($3::uuid[]))`, bucketID, scope.All, scope.IDs).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT ` + artifactScopeSelectColumns + `
		FROM fused_artifact_scopes s
		JOIN fused_artifact_buckets b ON s.artifact_id = b.artifact_id
		WHERE b.bucket_id = $1 AND ($2 OR s.artifact_id = ANY($3::uuid[]))
		ORDER BY s.created_at DESC
		LIMIT $4 OFFSET $5
	`
	rows, err := s.db.Query(ctx, query, bucketID, scope.All, scope.IDs, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var scopes []ArtifactScope
	for rows.Next() {
		scope, err := scanArtifactScope(rows)
		if err != nil {
			return nil, 0, err
		}
		scopes = append(scopes, *scope)
	}
	return scopes, total, rows.Err()
}
