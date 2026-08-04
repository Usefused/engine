package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

type WorkspaceService struct {
	ID               uuid.UUID
	ServiceID        uuid.UUID
	ServiceSlug      string
	Version          string
	ServiceVersionID uuid.UUID
	ServiceName      string
	AddedBy          uuid.UUID
	CreatedAt        time.Time
}

type WorkspaceServiceVersion struct {
	ID               uuid.UUID
	ServiceID        uuid.UUID
	Version          string
	ServiceVersionID uuid.UUID
	Status           string
	EnabledBy        uuid.UUID
	CreatedAt        time.Time
	EnabledAt        time.Time
}

type workspaceServiceWriter interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (s *postgresStore) AddWorkspaceServiceVersion(
	ctx context.Context,
	serviceID uuid.UUID,
	serviceSlug string,
	version string,
	serviceVersionID uuid.UUID,
	serviceName string,
	enabledBy uuid.UUID,
) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return ErrWorkspaceServiceVersionRequired
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("AddWorkspaceServiceVersion: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := upsertWorkspaceService(ctx, tx, serviceID, serviceSlug, serviceName, enabledBy); err != nil {
		return err
	}
	if err := enableWorkspaceServiceVersion(ctx, tx, serviceID, version, serviceVersionID, enabledBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("AddWorkspaceServiceVersion: commit: %w", err)
	}
	return nil
}

func (s *postgresStore) EnableWorkspaceServiceVersion(
	ctx context.Context,
	serviceID uuid.UUID,
	version string,
	serviceVersionID uuid.UUID,
	enabledBy uuid.UUID,
) error {
	return enableWorkspaceServiceVersion(ctx, s.db, serviceID, version, serviceVersionID, enabledBy)
}

func (s *postgresStore) DisableWorkspaceServiceVersion(
	ctx context.Context,
	serviceID uuid.UUID,
	version string,
) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return ErrWorkspaceServiceVersionRequired
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("DisableWorkspaceServiceVersion: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := deleteWorkspaceServiceVersion(ctx, tx, serviceID, version); err != nil {
		return err
	}
	if err := deleteWorkspaceServiceIfNoVersions(ctx, tx, serviceID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("DisableWorkspaceServiceVersion: commit: %w", err)
	}
	return nil
}

func (s *postgresStore) ListWorkspaceServiceVersions(
	ctx context.Context,
	serviceID uuid.UUID,
) ([]WorkspaceServiceVersion, error) {
	grouped, err := s.ListWorkspaceServiceVersionsForServices(ctx, []uuid.UUID{serviceID})
	if err != nil {
		return nil, err
	}
	return grouped[serviceID], nil
}

func (s *postgresStore) ListWorkspaceServiceVersionsForServices(
	ctx context.Context,
	serviceIDs []uuid.UUID,
) (map[uuid.UUID][]WorkspaceServiceVersion, error) {
	out := map[uuid.UUID][]WorkspaceServiceVersion{}
	if len(serviceIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, listWorkspaceServiceVersionsSQL, serviceIDs)
	if err != nil {
		return nil, fmt.Errorf("ListWorkspaceServiceVersionsForServices: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		version, err := scanWorkspaceServiceVersion(rows)
		if err != nil {
			return nil, err
		}
		out[version.ServiceID] = append(out[version.ServiceID], version)
	}
	return out, rows.Err()
}

func (s *postgresStore) ListWorkspaceServices(
	ctx context.Context,
	names []string,
) ([]WorkspaceService, error) {
	query, args := listWorkspaceServicesQuery(names)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListWorkspaceServices: query: %w", err)
	}
	defer rows.Close()
	var services []WorkspaceService
	for rows.Next() {
		service, err := scanWorkspaceService(rows)
		if err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, rows.Err()
}

func (s *postgresStore) ListAuthorizedWorkspaceServices(ctx context.Context, scope accesscontrol.AuthorizedScope, names []string) ([]WorkspaceService, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, nil
	}
	query := listWorkspaceServicesSQL + `
		` + authorizedWorkspaceServicesWhereSQL + `
		ORDER BY s.created_at DESC`
	rows, err := s.db.Query(ctx, query, scope.All, scope.IDs, names)
	if err != nil {
		return nil, fmt.Errorf("ListAuthorizedWorkspaceServices: query: %w", err)
	}
	defer rows.Close()
	var services []WorkspaceService
	for rows.Next() {
		service, err := scanWorkspaceService(rows)
		if err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, rows.Err()
}

func (s *postgresStore) ResolveWorkspaceServiceIDsByKeys(ctx context.Context, keys []string) (map[string]uuid.UUID, error) {
	if len(keys) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (input.key) input.key, service.service_id
		FROM unnest($1::text[]) AS input(key)
		JOIN fused_workspace_services service
		  ON service.service_name = input.key
		  OR service.service_slug = input.key
		  OR service.service_slug = CASE
			WHEN input.key LIKE '@%/%' THEN split_part(input.key, '/', 2)
			ELSE input.key
		  END
		ORDER BY input.key, service.created_at DESC`, keys)
	if err != nil {
		return nil, fmt.Errorf("ResolveWorkspaceServiceIDsByKeys: query: %w", err)
	}
	defer rows.Close()
	resolved := make(map[string]uuid.UUID, len(keys))
	for rows.Next() {
		var key string
		var serviceID uuid.UUID
		if err := rows.Scan(&key, &serviceID); err != nil {
			return nil, fmt.Errorf("ResolveWorkspaceServiceIDsByKeys: scan: %w", err)
		}
		resolved[key] = serviceID
	}
	return resolved, rows.Err()
}

// ListWorkspaceServicesPage pushes pagination to the DB to avoid pulling all services into memory.
// It uses two queries (COUNT and SELECT) to satisfy the total items and the data for the given page.
// The same WHERE clause logic is shared between the two queries via listWorkspaceServicesQuery.
func (s *postgresStore) ListWorkspaceServicesPage(ctx context.Context, names []string, limit, offset int) ([]WorkspaceService, int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.workspace_services.list_page")
	defer span.End()

	// 1. Get total count matching the names filter.
	countSQL := "SELECT COUNT(*) FROM fused_workspace_services s"
	var countArgs []any
	if len(names) > 0 {
		countSQL += " WHERE COALESCE(s.service_name, '') = ANY($1)"
		countArgs = append(countArgs, names)
	}
	var total int
	if err := s.db.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("ListWorkspaceServicesPage count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// 2. Fetch the paginated data using a CTE to ensure LATERAL JOIN is only run for the current page.
	query := `
		WITH paged_services AS (
			SELECT id, service_id, service_slug, service_name, added_by, created_at
			FROM fused_workspace_services s`
	var args []any
	if len(names) > 0 {
		query += "\n\t\t\tWHERE COALESCE(s.service_name, '') = ANY($1)"
		args = append(args, names)
	}
	query += fmt.Sprintf(`
			ORDER BY created_at DESC
			LIMIT $%d OFFSET $%d
		)
		SELECT s.id, s.service_id, COALESCE(s.service_slug, ''),
		       COALESCE(latest.version, ''),
		       COALESCE(latest.service_version_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(s.service_name, ''),
		       COALESCE(s.added_by, '00000000-0000-0000-0000-000000000000'::uuid),
		       s.created_at
		FROM paged_services s
		JOIN LATERAL (
			SELECT version, service_version_id
			FROM fused_workspace_service_versions
			WHERE service_id = s.service_id
			  AND status <> 'deprecated'
			ORDER BY enabled_at DESC, id DESC
			LIMIT 1
		) latest ON true
		ORDER BY s.created_at DESC`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("ListWorkspaceServicesPage query: %w", err)
	}
	defer rows.Close()

	var services []WorkspaceService
	for rows.Next() {
		service, err := scanWorkspaceService(rows)
		if err != nil {
			return nil, 0, err
		}
		services = append(services, service)
	}
	return services, total, rows.Err()
}

func (s *postgresStore) ListAuthorizedWorkspaceServicesPage(ctx context.Context, scope accesscontrol.AuthorizedScope, names []string, limit, offset int) ([]WorkspaceService, int, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_services s `+authorizedWorkspaceServicesWhereSQL, scope.All, scope.IDs, names).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("ListAuthorizedWorkspaceServicesPage count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	query := `WITH paged_services AS (
		SELECT id, service_id, service_slug, service_name, added_by, created_at
		FROM fused_workspace_services s ` + authorizedWorkspaceServicesWhereSQL + `
		ORDER BY created_at DESC LIMIT $4 OFFSET $5
	)
	SELECT s.id, s.service_id, COALESCE(s.service_slug, ''),
	       COALESCE(latest.version, ''),
	       COALESCE(latest.service_version_id, '00000000-0000-0000-0000-000000000000'::uuid),
	       COALESCE(s.service_name, ''),
	       COALESCE(s.added_by, '00000000-0000-0000-0000-000000000000'::uuid), s.created_at
	FROM paged_services s
	JOIN LATERAL (
		SELECT version, service_version_id FROM fused_workspace_service_versions
		WHERE service_id = s.service_id AND status <> 'deprecated'
		ORDER BY enabled_at DESC, id DESC LIMIT 1
	) latest ON true
	ORDER BY s.created_at DESC`
	rows, err := s.db.Query(ctx, query, scope.All, scope.IDs, names, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("ListAuthorizedWorkspaceServicesPage query: %w", err)
	}
	defer rows.Close()
	var services []WorkspaceService
	for rows.Next() {
		service, err := scanWorkspaceService(rows)
		if err != nil {
			return nil, 0, err
		}
		services = append(services, service)
	}
	return services, total, rows.Err()
}

// Why: CLI commands address services by Registry slug, while the Admin UI can
// filter using the display name. Keeping both exact matches in SQL avoids a
// false "not enabled" result without loading the workspace into Go memory.
const authorizedWorkspaceServicesWhereSQL = `WHERE ($1 OR s.service_id = ANY($2::uuid[]))
	AND (
		COALESCE(cardinality($3::text[]), 0) = 0
		OR COALESCE(s.service_name, '') = ANY($3::text[])
		OR COALESCE(s.service_slug, '') = ANY($3::text[])
		OR EXISTS (
			SELECT 1 FROM unnest($3::text[]) AS requested(name)
			WHERE requested.name LIKE '@%/%'
			  AND COALESCE(s.service_slug, '') = split_part(requested.name, '/', 2)
		)
	)`

func (s *postgresStore) ListBucketServiceSummaries(ctx context.Context, bucketID uuid.UUID, search string, limit, offset int) ([]BucketServiceSummary, int, error) {
	return s.ListAuthorizedBucketServiceSummaries(ctx, bucketID, accesscontrol.AuthorizedScope{All: true}, search, limit, offset)
}

func (s *postgresStore) ListAuthorizedBucketServiceSummaries(ctx context.Context, bucketID uuid.UUID, scope accesscontrol.AuthorizedScope, search string, limit, offset int) ([]BucketServiceSummary, int, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	search = strings.TrimSpace(search)
	var total int
	if err := s.db.QueryRow(ctx, bucketServiceSummaryCountSQL, bucketID, scope.All, scope.IDs, search).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("ListBucketServiceSummaries: count: %w", err)
	}
	rows, err := s.db.Query(ctx, bucketServiceSummaryPageSQL, bucketID, scope.All, scope.IDs, search, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("ListBucketServiceSummaries: query: %w", err)
	}
	defer rows.Close()
	items, err := collectBucketServiceSummaries(rows)
	return items, total, err
}

func (s *postgresStore) RemoveWorkspaceService(ctx context.Context, serviceID uuid.UUID) error {
	res, err := s.db.Exec(ctx, `DELETE FROM fused_workspace_services WHERE service_id = $1`, serviceID)
	if err != nil {
		return fmt.Errorf("RemoveWorkspaceService: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("RemoveWorkspaceService: %w", ErrWorkspaceServiceNotFound)
	}
	return nil
}

func collectBucketServiceSummaries(rows pgx.Rows) ([]BucketServiceSummary, error) {
	var items []BucketServiceSummary
	for rows.Next() {
		var item BucketServiceSummary
		if err := rows.Scan(
			&item.ServiceID, &item.ServiceName, &item.SecretCount, &item.ValueCount,
			&item.ConnectConfigCount, &item.ConnectedUserCount,
		); err != nil {
			return nil, fmt.Errorf("ListBucketServiceSummaries: scan: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *postgresStore) IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM fused_workspace_services
			WHERE service_id = $1
		)
	`, serviceID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("IsWorkspaceServiceEnabled: %w", err)
	}
	return exists, nil
}

// IsWorkspaceServiceVersionActive checks the exact routing tuple in SQL so a
// profile mutation never loads every enabled version merely to find one row.
func (s *postgresStore) IsWorkspaceServiceVersionActive(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM fused_workspace_service_versions
			WHERE service_id = $1
			  AND service_version_id = $2
			  AND status <> 'deprecated'
		)`, serviceID, serviceVersionID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("IsWorkspaceServiceVersionActive: %w", err)
	}
	return active, nil
}

func (s *postgresStore) GetWorkspaceServiceVersion(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceServiceVersion, error) {
	version, err := scanWorkspaceServiceVersionRow(s.db.QueryRow(ctx, workspaceServiceVersionByIDSQL, serviceID, serviceVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("workspace service version not found for service %s version %s: %w", serviceID, serviceVersionID, ErrWorkspaceServiceVersionNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("GetWorkspaceServiceVersion: %w", err)
	}
	return &version, nil
}

func (s *postgresStore) ListWorkspaceServiceVersionsMissingContractSnapshots(ctx context.Context, limit int) ([]WorkspaceServiceVersion, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, workspaceServiceVersionsMissingContractSnapshotsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("ListWorkspaceServiceVersionsMissingContractSnapshots: %w", err)
	}
	defer rows.Close()
	return collectWorkspaceServiceVersions(rows)
}

func (s *postgresStore) GetLatestWorkspaceServiceVersionByWorkspace(
	ctx context.Context,
	serviceID uuid.UUID,
) (string, error) {
	var version string
	err := s.db.QueryRow(ctx, latestWorkspaceServiceVersionSQL, serviceID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("workspace service version not found for service %s: %w", serviceID, ErrWorkspaceServiceNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("GetLatestWorkspaceServiceVersionByWorkspace: %w", err)
	}
	return version, nil
}

// GetLatestWorkspaceServiceVersionIDByWorkspace is
// GetLatestWorkspaceServiceVersionByWorkspace's sibling returning the
// service_version_id UUID -- the key a snapshot/execution-policy lookup
// needs, as opposed to the human-readable version name.
func (s *postgresStore) GetLatestWorkspaceServiceVersionIDByWorkspace(
	ctx context.Context,
	serviceID uuid.UUID,
) (uuid.UUID, error) {
	var versionID uuid.UUID
	err := s.db.QueryRow(ctx, latestWorkspaceServiceVersionIDSQL, serviceID).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("workspace service version not found for service %s: %w", serviceID, ErrWorkspaceServiceNotFound)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("GetLatestWorkspaceServiceVersionIDByWorkspace: %w", err)
	}
	return versionID, nil
}

func upsertWorkspaceService(
	ctx context.Context,
	db workspaceServiceWriter,
	serviceID uuid.UUID,
	serviceSlug string,
	serviceName string,
	addedBy uuid.UUID,
) error {
	_, err := db.Exec(ctx, upsertWorkspaceServiceSQL, serviceID, serviceSlug, serviceName, addedBy)
	return err
}

const upsertWorkspaceServiceSQL = `
		INSERT INTO fused_workspace_services (service_id, service_slug, service_name, added_by)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4)
		ON CONFLICT (service_id) DO UPDATE SET
			-- Some activation callers only know the version tuple. Preserve the
			-- existing display metadata so those refresh/activation paths cannot
			-- make bucket-linked services fall back to raw UUIDs.
			service_slug = COALESCE(EXCLUDED.service_slug, fused_workspace_services.service_slug),
			service_name = COALESCE(EXCLUDED.service_name, fused_workspace_services.service_name),
			added_by = EXCLUDED.added_by
	`

func enableWorkspaceServiceVersion(
	ctx context.Context,
	db workspaceServiceWriter,
	serviceID uuid.UUID,
	version string,
	serviceVersionID uuid.UUID,
	enabledBy uuid.UUID,
) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return ErrWorkspaceServiceVersionRequired
	}
	_, err := db.Exec(ctx, `
		INSERT INTO fused_workspace_service_versions (service_id, version, service_version_id, enabled_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (service_id, service_version_id) DO UPDATE SET
			version = EXCLUDED.version,
			enabled_by = EXCLUDED.enabled_by
	`, serviceID, version, serviceVersionID, enabledBy)
	if err != nil {
		return fmt.Errorf("EnableWorkspaceServiceVersion: %w", err)
	}
	return nil
}

func deleteWorkspaceServiceVersion(
	ctx context.Context,
	db workspaceServiceWriter,
	serviceID uuid.UUID,
	version string,
) error {
	res, err := db.Exec(ctx, `
		DELETE FROM fused_workspace_service_versions
		WHERE service_id = $1 AND version = $2
	`, serviceID, version)
	if err != nil {
		return fmt.Errorf("DisableWorkspaceServiceVersion: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("DisableWorkspaceServiceVersion: %w", ErrWorkspaceServiceVersionNotFound)
	}
	return nil
}

func deleteWorkspaceServiceIfNoVersions(
	ctx context.Context,
	db workspaceServiceWriter,
	serviceID uuid.UUID,
) error {
	_, err := db.Exec(ctx, `
		DELETE FROM fused_workspace_services s
		WHERE s.service_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM fused_workspace_service_versions v
			WHERE v.service_id = s.service_id
		  )
	`, serviceID)
	return err
}

func listWorkspaceServicesQuery(names []string) (string, []any) {
	query := listWorkspaceServicesSQL
	var args []any
	if len(names) > 0 {
		// Registry references may be account-qualified while this singleton
		// workspace stores the verified bare slug. Normalize each requested key
		// inside the one SQL query so authorization cannot drift or become N+1.
		query += ` WHERE EXISTS (
			SELECT 1 FROM unnest($1::text[]) AS input(key)
			WHERE COALESCE(s.service_name, '') = input.key
			   OR COALESCE(s.service_slug, '') = input.key
			   OR COALESCE(s.service_slug, '') = CASE
				 WHEN input.key LIKE '@%/%' THEN split_part(input.key, '/', 2)
				 ELSE input.key
			   END
		)`
		args = append(args, names)
	}
	return query + " ORDER BY s.created_at DESC", args
}

func scanWorkspaceService(rows pgx.Rows) (WorkspaceService, error) {
	var service WorkspaceService
	err := rows.Scan(
		&service.ID, &service.ServiceID, &service.ServiceSlug,
		&service.Version, &service.ServiceVersionID, &service.ServiceName,
		&service.AddedBy, &service.CreatedAt,
	)
	if err != nil {
		return service, fmt.Errorf("ListWorkspaceServices: scan: %w", err)
	}
	return service, nil
}

func scanWorkspaceServiceVersion(rows pgx.Rows) (WorkspaceServiceVersion, error) {
	return scanWorkspaceServiceVersionRow(rows)
}

func collectWorkspaceServiceVersions(rows pgx.Rows) ([]WorkspaceServiceVersion, error) {
	var versions []WorkspaceServiceVersion
	for rows.Next() {
		version, err := scanWorkspaceServiceVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

type workspaceServiceVersionScanner interface {
	Scan(dest ...any) error
}

func scanWorkspaceServiceVersionRow(row workspaceServiceVersionScanner) (WorkspaceServiceVersion, error) {
	var version WorkspaceServiceVersion
	err := row.Scan(
		&version.ID, &version.ServiceID,
		&version.Version, &version.ServiceVersionID, &version.Status,
		&version.EnabledBy, &version.CreatedAt, &version.EnabledAt,
	)
	if err != nil {
		return version, fmt.Errorf("ListWorkspaceServiceVersionsForServices: scan: %w", err)
	}
	return version, nil
}

const listWorkspaceServicesSQL = `
	SELECT s.id, s.service_id, COALESCE(s.service_slug, ''),
	       COALESCE(latest.version, ''),
	       COALESCE(latest.service_version_id, '00000000-0000-0000-0000-000000000000'::uuid),
	       COALESCE(s.service_name, ''),
	       COALESCE(s.added_by, '00000000-0000-0000-0000-000000000000'::uuid),
	       s.created_at
	FROM fused_workspace_services s
	JOIN LATERAL (
		SELECT version, service_version_id
		FROM fused_workspace_service_versions
		WHERE service_id = s.service_id
		  AND status <> 'deprecated'
		ORDER BY enabled_at DESC, id DESC
		LIMIT 1
	) latest ON true`

// Bucket service summaries aggregate context across all resources linked to a bucket.
const bucketServiceIDsSQL = `
	WITH bucket_service_ids AS (
		SELECT service_id FROM fused_workspace_secrets WHERE bucket_id = $1 AND ($2 OR service_id = ANY($3::uuid[]))
		UNION
		SELECT service_id FROM fused_connect_configs WHERE bucket_id = $1 AND ($2 OR service_id = ANY($3::uuid[]))
		UNION
		SELECT service_id FROM fused_auth_connections WHERE bucket_id = $1 AND ($2 OR service_id = ANY($3::uuid[]))
		UNION
		SELECT service_id FROM fused_bucket_values WHERE bucket_id = $1 AND ($2 OR service_id = ANY($3::uuid[]))
	)`

const bucketServiceSummaryCountSQL = bucketServiceIDsSQL + `
	SELECT COUNT(*)
	FROM bucket_service_ids ids
	LEFT JOIN fused_workspace_services ws
	  ON ws.service_id = ids.service_id
	WHERE $4 = ''
	   OR COALESCE(ws.service_name, '') ILIKE '%' || $4 || '%'
	   OR ids.service_id::text ILIKE '%' || $4 || '%'`

const bucketServiceSummaryPageSQL = bucketServiceIDsSQL + `,
	secret_counts AS (
		SELECT service_id, COUNT(*) AS secret_count
		FROM fused_workspace_secrets
		WHERE bucket_id = $1
		GROUP BY service_id
	),
	value_counts AS (
		SELECT service_id, COUNT(*) AS value_count
		FROM fused_bucket_values
		WHERE bucket_id = $1
		GROUP BY service_id
	),
	connect_config_counts AS (
		SELECT service_id, COUNT(*) AS connect_config_count
		FROM fused_connect_configs
		WHERE bucket_id = $1
		GROUP BY service_id
	),
	connected_user_counts AS (
		SELECT service_id, COUNT(DISTINCT end_user_ref) AS connected_user_count
		FROM fused_auth_connections
		WHERE bucket_id = $1
		GROUP BY service_id
	)
	SELECT ids.service_id,
	       COALESCE(ws.service_name, ''),
	       COALESCE(secret_counts.secret_count, 0),
	       COALESCE(value_counts.value_count, 0),
	       COALESCE(connect_config_counts.connect_config_count, 0),
	       COALESCE(connected_user_counts.connected_user_count, 0)
	FROM bucket_service_ids ids
	LEFT JOIN fused_workspace_services ws
	  ON ws.service_id = ids.service_id
	LEFT JOIN secret_counts ON secret_counts.service_id = ids.service_id
	LEFT JOIN value_counts ON value_counts.service_id = ids.service_id
	LEFT JOIN connect_config_counts ON connect_config_counts.service_id = ids.service_id
	LEFT JOIN connected_user_counts ON connected_user_counts.service_id = ids.service_id
	WHERE $4 = ''
	   OR COALESCE(ws.service_name, '') ILIKE '%' || $4 || '%'
	   OR ids.service_id::text ILIKE '%' || $4 || '%'
	ORDER BY COALESCE(ws.service_name, ''), ids.service_id
	LIMIT $5 OFFSET $6`

const listWorkspaceServiceVersionsSQL = `
	SELECT id, service_id, version, service_version_id, status,
	       COALESCE(enabled_by, '00000000-0000-0000-0000-000000000000'::uuid),
	       created_at, enabled_at
	FROM fused_workspace_service_versions
	WHERE service_id = ANY($1::uuid[])
	ORDER BY enabled_at DESC, id DESC`

const latestWorkspaceServiceVersionSQL = `
	SELECT version
	FROM fused_workspace_service_versions
	WHERE service_id = $1
	  AND status <> 'deprecated'
	ORDER BY enabled_at DESC, id DESC
	LIMIT 1`

const latestWorkspaceServiceVersionIDSQL = `
	SELECT service_version_id
	FROM fused_workspace_service_versions
	WHERE service_id = $1
	  AND status <> 'deprecated'
	ORDER BY enabled_at DESC, id DESC
	LIMIT 1`

const workspaceServiceVersionByIDSQL = `
	SELECT id, service_id, version, service_version_id, status,
	       COALESCE(enabled_by, '00000000-0000-0000-0000-000000000000'::uuid),
	       created_at, enabled_at
	FROM fused_workspace_service_versions
	WHERE service_id = $1
	  AND service_version_id = $2
	  AND status <> 'deprecated'`

const workspaceServiceVersionsMissingContractSnapshotsSQL = `
	SELECT versions.id, versions.service_id, versions.version,
	       versions.service_version_id, versions.status,
	       COALESCE(versions.enabled_by, '00000000-0000-0000-0000-000000000000'::uuid),
	       versions.created_at, versions.enabled_at
	FROM fused_workspace_service_versions versions
	LEFT JOIN fused_service_contract_snapshots snapshots
	  ON snapshots.service_version_id = versions.service_version_id
	WHERE versions.status <> 'deprecated'
	  AND snapshots.id IS NULL
	ORDER BY versions.enabled_at ASC, versions.id ASC
	LIMIT $1`

var ErrWorkspaceServiceNotFound = errors.New("workspace service not found")

var ErrWorkspaceServiceVersionNotFound = errors.New("workspace service version not found")

// ErrWorkspaceServiceVersionRequired prevents runtime dispatch from floating to
// whichever Registry version happens to be latest at execution time.
var ErrWorkspaceServiceVersionRequired = errors.New("workspace service version is required")
