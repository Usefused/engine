package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_buckets`).Scan(&total); err != nil {
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
		ORDER BY b.is_default DESC, b.updated_at DESC, b.created_at DESC
		LIMIT $1 OFFSET $2
	`
	rows, err := s.db.Query(ctx, query, limit, offset)
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

func (s *postgresStore) DeleteBucket(ctx context.Context, name string) error {
	query := `DELETE FROM fused_buckets WHERE name = $1 AND is_default = false`
	tag, err := s.db.Exec(ctx, query, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("bucket not found or is default")
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

func (s *postgresStore) LinkBucketToSDK(ctx context.Context, artifactID, bucketID uuid.UUID) error {
	// The no-op update makes linking the same bucket idempotent in one query,
	// while the WHERE clause rejects attempts to retarget an existing artifact.
	var linkedBucketID uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO fused_artifact_buckets (artifact_id, bucket_id) VALUES ($1, $2)
		ON CONFLICT (artifact_id) DO UPDATE SET bucket_id = fused_artifact_buckets.bucket_id
		WHERE fused_artifact_buckets.bucket_id = EXCLUDED.bucket_id
		RETURNING bucket_id`, artifactID, bucketID).Scan(&linkedBucketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSDKBucketImmutable
	}
	return err
}

func (s *postgresStore) UnlinkBucketFromSDK(ctx context.Context, artifactID, bucketID uuid.UUID) error {
	query := `DELETE FROM fused_artifact_buckets WHERE artifact_id = $1 AND bucket_id = $2`
	_, err := s.db.Exec(ctx, query, artifactID, bucketID)
	return err
}

func (s *postgresStore) ListBucketsForSDK(ctx context.Context, artifactID uuid.UUID) ([]Bucket, error) {
	query := `
		SELECT b.id, b.name, b.is_default, b.created_at, b.updated_at
		FROM fused_buckets b
		JOIN fused_artifact_buckets sb ON b.id = sb.bucket_id
		WHERE sb.artifact_id = $1
		ORDER BY b.created_at ASC
	`
	rows, err := s.db.Query(ctx, query, artifactID)
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
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_artifact_buckets WHERE bucket_id = $1`, bucketID).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT ` + artifactScopeSelectColumns + `
		FROM fused_artifact_scopes s
		JOIN fused_artifact_buckets b ON s.artifact_id = b.artifact_id
		WHERE b.bucket_id = $1
		ORDER BY s.created_at DESC
		LIMIT $2 OFFSET $3
	`
	rows, err := s.db.Query(ctx, query, bucketID, limit, offset)
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
