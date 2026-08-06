package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
)

type AppCatalogItem struct {
	AppFamilyID           uuid.UUID
	AppID                 uuid.UUID
	Name                  string
	Description           string
	Version               string
	Kind                  string
	Status                string
	CreatedAt             time.Time
	TargetLanguage        string
	GeneratorVersion      string
	Readme                string
	Selections            []models.SDKSelection
	PlannedDeactivationAt *time.Time
}

type AppServiceSummary struct {
	ServiceID     uuid.UUID
	ServiceSlug   string
	ServiceName   string
	Version       string
	SelectAll     bool
	EndpointCount int
	WebhookCount  int
}

type AppCatalogRepository interface {
	ListAuthorizedAppsByAccount(context.Context, uuid.UUID, accesscontrol.AuthorizedScope, string, string, string, int, int) ([]AppCatalogItem, int, error)
	GetAuthorizedApp(context.Context, uuid.UUID, uuid.UUID, accesscontrol.AuthorizedScope) (*AppCatalogItem, error)
	ListAuthorizedAppsByFamily(context.Context, uuid.UUID, uuid.UUID, accesscontrol.AuthorizedScope) ([]AppCatalogItem, error)
	ListAuthorizedAppServiceSummaries(context.Context, uuid.UUID, uuid.UUID, accesscontrol.AuthorizedScope) ([]AppServiceSummary, error)
}

const appCatalogSelect = `
	SELECT app.app_family_id, app.app_id, family.display_name,
	       COALESCE(plan.resolved_payload->>'description', ''), app.version,
	       family.kind, app.status, app.created_at, COALESCE(family.target_language, ''),
	       COALESCE(app.generator_version, ''), COALESCE(plan.resolved_payload->>'readme', ''), app.selections,
	       app.planned_deactivation_at
	FROM fused_apps app
	JOIN fused_app_families family
	  ON family.app_family_id = app.app_family_id AND family.account_id = app.account_id
	LEFT JOIN LATERAL (
		SELECT applied.resolved_payload
		FROM fused_config_plans applied
		WHERE applied.config_key = app.config_key AND applied.source_hash = app.source_hash
		  AND applied.status = 'applied'
		ORDER BY applied.applied_at DESC NULLS LAST, applied.created_at DESC
		LIMIT 1
	) plan ON true`

func (s *postgresStore) ListAuthorizedAppsByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind, search, version string, limit, offset int) ([]AppCatalogItem, int, error) {
	kind, valid := normalizeAppKind(kind)
	if !valid {
		return nil, 0, ErrInvalidAppKind
	}
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	rows, err := s.db.Query(ctx, appCatalogSelect+`
		WHERE app.account_id = $1 AND ($2 = '' OR family.kind = $2)
		  AND ($3 = '' OR family.display_name ILIKE '%' || $3 || '%')
		  AND ($4 = '' OR app.version = $4)
		  AND ($5 OR app.app_family_id = ANY($6::uuid[]))
		ORDER BY app.created_at DESC, app.app_id
		LIMIT $7 OFFSET $8`, accountID, kind, strings.TrimSpace(search), strings.TrimSpace(version), scope.All, scope.IDs, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()
	items, err := scanAppCatalogItems(rows)
	if err != nil {
		return nil, 0, err
	}
	var total int
	err = s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM fused_apps app
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id AND family.account_id = app.account_id
		WHERE app.account_id = $1 AND ($2 = '' OR family.kind = $2)
		  AND ($3 = '' OR family.display_name ILIKE '%' || $3 || '%')
		  AND ($4 = '' OR app.version = $4)
		  AND ($5 OR app.app_family_id = ANY($6::uuid[]))`, accountID, kind, strings.TrimSpace(search), strings.TrimSpace(version), scope.All, scope.IDs).Scan(&total)
	return items, total, err
}

func (s *postgresStore) GetAuthorizedApp(ctx context.Context, accountID, appID uuid.UUID, scope accesscontrol.AuthorizedScope) (*AppCatalogItem, error) {
	item, err := scanAppCatalogItem(s.db.QueryRow(ctx, appCatalogSelect+`
		WHERE app.account_id = $1 AND app.app_id = $2
		  AND ($3 OR app.app_family_id = ANY($4::uuid[]))`, accountID, appID, scope.All, scope.IDs))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	return item, err
}

func (s *postgresStore) ListAuthorizedAppsByFamily(ctx context.Context, accountID, appFamilyID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]AppCatalogItem, error) {
	rows, err := s.db.Query(ctx, appCatalogSelect+`
		WHERE app.account_id = $1 AND app.app_family_id = $2
		  AND ($3 OR app.app_family_id = ANY($4::uuid[]))
		ORDER BY app.created_at DESC, app.app_id
		LIMIT $5`, accountID, appFamilyID, scope.All, scope.IDs, appFamilyCollectionLimit)
	if err != nil {
		return nil, fmt.Errorf("list app versions: %w", err)
	}
	defer rows.Close()
	return scanAppCatalogItems(rows)
}

func (s *postgresStore) ListAuthorizedAppServiceSummaries(ctx context.Context, accountID, appID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]AppServiceSummary, error) {
	rows, err := s.db.Query(ctx, `
		SELECT service.service_id, COALESCE(service.service_slug, ''), COALESCE(service.service_name, ''),
		       COALESCE(version.version, ''), COALESCE((selection->>'select_all')::boolean, false),
		       jsonb_array_length(COALESCE(selection->'endpoint_ids', '[]'::jsonb)),
		       jsonb_array_length(COALESCE(selection->'webhook_ids', '[]'::jsonb))
		FROM fused_apps app
		CROSS JOIN LATERAL jsonb_array_elements(app.selections) selection
		JOIN fused_workspace_services service ON service.service_id = (selection->>'service_id')::uuid
		LEFT JOIN fused_workspace_service_versions version
		  ON version.service_id = service.service_id
		 AND version.service_version_id = NULLIF(selection->>'service_version_id', '')::uuid
		WHERE app.account_id = $1 AND app.app_id = $2
		  AND ($3 OR app.app_family_id = ANY($4::uuid[]))
		ORDER BY COALESCE(service.service_slug, service.service_name), service.service_id`, accountID, appID, scope.All, scope.IDs)
	if err != nil {
		return nil, fmt.Errorf("list app services: %w", err)
	}
	defer rows.Close()
	services := make([]AppServiceSummary, 0)
	for rows.Next() {
		var service AppServiceSummary
		if err := rows.Scan(&service.ServiceID, &service.ServiceSlug, &service.ServiceName, &service.Version, &service.SelectAll, &service.EndpointCount, &service.WebhookCount); err != nil {
			return nil, fmt.Errorf("scan app service: %w", err)
		}
		services = append(services, service)
	}
	return services, rows.Err()
}

type appCatalogScanner interface{ Scan(...any) error }

func scanAppCatalogItem(row appCatalogScanner) (*AppCatalogItem, error) {
	var item AppCatalogItem
	var selections json.RawMessage
	err := row.Scan(&item.AppFamilyID, &item.AppID, &item.Name, &item.Description,
		&item.Version, &item.Kind, &item.Status, &item.CreatedAt, &item.TargetLanguage,
		&item.GeneratorVersion, &item.Readme, &selections, &item.PlannedDeactivationAt)
	if err == nil {
		err = json.Unmarshal(selections, &item.Selections)
	}
	return &item, err
}

func scanAppCatalogItems(rows pgx.Rows) ([]AppCatalogItem, error) {
	items := make([]AppCatalogItem, 0)
	for rows.Next() {
		item, err := scanAppCatalogItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

var _ AppCatalogRepository = (*postgresStore)(nil)
