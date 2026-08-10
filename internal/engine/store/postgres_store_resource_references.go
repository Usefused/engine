package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *postgresStore) ResolveResourceReference(ctx context.Context, query ResourceReferenceQuery) (uuid.UUID, error) {
	if err := validateResourceReferenceQuery(query); err != nil {
		return uuid.Nil, err
	}
	value := strings.TrimSpace(query.Value)
	exactID, _ := uuid.Parse(value)
	switch query.Kind {
	case ReferenceApp:
		return s.resolveAppVersionReference(ctx, exactID, value, query.AppVersion, query.AppKind.String(), query.AllowedAll, query.AllowedIDs)
	case ReferenceAppFamily:
		return s.resolveAppReference(ctx, exactID, value, query.AppKind.String(), query.AllowedAll, query.AllowedIDs)
	default:
		return s.resolveNonAppReference(ctx, query, exactID, value)
	}
}

func (s *postgresStore) resolveNonAppReference(ctx context.Context, query ResourceReferenceQuery, exactID uuid.UUID, value string) (uuid.UUID, error) {
	var id uuid.UUID
	var err error
	switch query.Kind {
	case ReferenceTeam:
		err = s.db.QueryRow(ctx, resolveTeamReferenceSQL, exactID, value, query.AllowedAll, query.AllowedIDs).Scan(&id)
	case ReferenceUser:
		err = s.db.QueryRow(ctx, resolveUserReferenceSQL, exactID, value, query.AllowedAll).Scan(&id)
	case ReferenceService:
		err = s.db.QueryRow(ctx, resolveServiceReferenceSQL, exactID, value, query.AllowedAll, query.AllowedIDs).Scan(&id)
	case ReferenceBucket:
		err = s.db.QueryRow(ctx, resolveBucketReferenceSQL, exactID, value, query.AllowedAll, query.AllowedIDs).Scan(&id)
	case ReferenceCredential:
		err = s.db.QueryRow(ctx, resolveCredentialReferenceSQL, query.ParentID, exactID, value, query.AllowedAll).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: %s %q", ErrResourceReferenceNotFound, query.Kind, value)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve %s reference: %w", query.Kind, err)
	}
	return id, nil
}

func (s *postgresStore) resolveAppVersionReference(ctx context.Context, exactID uuid.UUID, value, version, appKind string, allowedAll bool, allowedFamilyIDs []uuid.UUID) (uuid.UUID, error) {
	if exactID != uuid.Nil {
		var id uuid.UUID
		err := s.db.QueryRow(ctx, `
			SELECT app.app_id FROM fused_apps app
			JOIN fused_app_families family ON family.app_family_id = app.app_family_id
			WHERE app.app_id = $1 AND ($2 = '' OR family.kind = $2)
			  AND ($3 OR family.app_family_id = ANY($4::uuid[]))
		`, exactID, appKind, allowedAll, allowedFamilyIDs).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("%w: app %q", ErrResourceReferenceNotFound, value)
		}
		if err != nil {
			return uuid.Nil, fmt.Errorf("resolve app UUID: %w", err)
		}
		return id, nil
	}
	name := strings.TrimSpace(value)
	if name == "" || strings.TrimSpace(version) == "" {
		return uuid.Nil, fmt.Errorf("%w: app version is required", ErrResourceReferenceNotFound)
	}
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT app.app_id FROM fused_apps app
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id
		WHERE (lower(family.canonical_name) = lower($1) OR lower(family.display_name) = lower($1))
		  AND app.version = $2 AND ($3 = '' OR family.kind = $3)
		  AND ($4 OR family.app_family_id = ANY($5::uuid[]))
		ORDER BY app.created_at DESC, app.app_id DESC LIMIT 1
	`, name, version, appKind, allowedAll, allowedFamilyIDs).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: app %q", ErrResourceReferenceNotFound, value)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve app version reference: %w", err)
	}
	return id, nil
}

func (s *postgresStore) resolveAppReference(ctx context.Context, exactID uuid.UUID, value, appKind string, allowedAll bool, allowedIDs []uuid.UUID) (uuid.UUID, error) {
	if exactID != uuid.Nil {
		var id uuid.UUID
		if err := s.db.QueryRow(ctx, `SELECT app_family_id FROM fused_app_families WHERE app_family_id = $1 AND ($2 = '' OR kind = $2) AND ($3 OR app_family_id = ANY($4::uuid[]))`, exactID, appKind, allowedAll, allowedIDs).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return uuid.Nil, fmt.Errorf("%w: app %q", ErrResourceReferenceNotFound, value)
			}
			return uuid.Nil, fmt.Errorf("resolve app UUID: %w", err)
		}
		return id, nil
	}
	name := strings.TrimSpace(value)
	if name == "" {
		return uuid.Nil, fmt.Errorf("%w: invalid app reference %q", ErrResourceReferenceNotFound, value)
	}
	var id uuid.UUID
	var kindCount int
	err := s.db.QueryRow(ctx, resolveAppReferenceSQL, name, appKind, allowedAll, allowedIDs).Scan(&id, &kindCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: app %q", ErrResourceReferenceNotFound, value)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve app reference: %w", err)
	}
	// A generic RBAC selector cannot silently choose between an SDK and an MCP
	// server that happen to share a name. Kind-specific product commands never
	// hit this branch; generic access commands can use the displayed full UUID.
	if kindCount > 1 {
		return uuid.Nil, fmt.Errorf("%w: app %q exists as both an SDK and MCP server", ErrResourceReferenceAmbiguous, value)
	}
	return id, nil
}

const resolveTeamReferenceSQL = `
SELECT id FROM fused_teams
WHERE (id = $1 OR lower(slug) = lower($2))
	AND ($3 OR id = ANY($4::uuid[]))
ORDER BY (id = $1) DESC LIMIT 1`

const resolveUserReferenceSQL = `
SELECT subject_id FROM fused_users
WHERE (subject_id = $1 OR email_normalized = lower($2)) AND $3
ORDER BY (subject_id = $1) DESC LIMIT 1`

const resolveServiceReferenceSQL = `
SELECT service_id FROM fused_workspace_services
WHERE (service_id = $1 OR lower(service_slug) = lower($2))
	AND ($3 OR service_id = ANY($4::uuid[]))
ORDER BY (service_id = $1) DESC LIMIT 1`

const resolveBucketReferenceSQL = `
SELECT id FROM fused_buckets
WHERE (id = $1 OR lower(name) = lower($2))
	AND ($3 OR id = ANY($4::uuid[]))
ORDER BY (id = $1) DESC LIMIT 1`

const resolveCredentialReferenceSQL = `
SELECT credential.id FROM fused_control_credentials credential
JOIN fused_users person ON person.subject_id = credential.subject_id
WHERE person.subject_id = $1 AND $4 AND (credential.id = $2 OR (credential.revoked_at IS NULL AND lower(credential.name) = lower($3)))
ORDER BY (credential.id = $2) DESC, credential.created_at DESC LIMIT 1`

const resolveAppReferenceSQL = `
WITH matches AS (
	SELECT app_family_id, kind, created_at FROM fused_app_families
	WHERE (lower(canonical_name) = lower($1) OR lower(display_name) = lower($1))
		AND ($2 = '' OR kind = $2) AND ($3 OR app_family_id = ANY($4::uuid[]))
), selected AS (
	SELECT app_family_id FROM matches ORDER BY created_at DESC, app_family_id DESC LIMIT 1
)
SELECT app_family_id, (SELECT COUNT(DISTINCT kind) FROM matches) FROM selected`

var _ ResourceReferenceResolver = (*postgresStore)(nil)
