package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// AppFamilyCatalogItem describes one logical application and its latest presentation metadata without selecting execution.
type AppFamilyCatalogItem struct {
	AppFamilyID     uuid.UUID
	Name            string
	Kind            AppKind
	TargetLanguage  string
	VersionCount    int
	LatestAppID     uuid.UUID
	LatestVersion   string
	LatestStatus    AppStatus
	LatestCreatedAt *time.Time
	StableAppID     uuid.UUID
	StableVersion   string
}

// AppFamilyCatalogRepository keeps application grouping and authorized pagination in Engine persistence.
type AppFamilyCatalogRepository interface {
	ListAuthorizedAppFamilies(context.Context, uuid.UUID, accesscontrol.AuthorizedScope, string, string, int, int) ([]AppFamilyCatalogItem, int, error)
}

const appFamilyCatalogWhere = `
 WHERE family.account_id = $1 AND ($2 = '' OR family.kind = $2)
 AND ($3 = '' OR family.display_name ILIKE '%' || $3 || '%')
 AND ($4 OR family.app_family_id = ANY($5::uuid[]))`

// ListAuthorizedAppFamilies filters account and RBAC scope before grouping versions, ordering, and pagination.
func (s *postgresStore) ListAuthorizedAppFamilies(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind, search string, limit, offset int) ([]AppFamilyCatalogItem, int, error) {
	kind, valid := normalizeAppKind(kind)
	// Invalid kinds must not silently broaden the catalogue to other adapters.
	if !valid {
		return nil, 0, ErrInvalidAppKind
	}
	// Empty grants cannot fall back to workspace-wide discovery or reveal totals.
	if !scope.All && len(scope.IDs) == 0 {
		return []AppFamilyCatalogItem{}, 0, nil
	}
	args := []any{accountID, kind, strings.TrimSpace(search), scope.All, scope.IDs}
	// One account-scoped window scan computes counts and the newest immutable row without a per-family lookup.
	rows, err := s.db.Query(ctx, `
 WITH version_catalog AS (
   SELECT app.app_family_id, app.account_id, app.app_id, app.version, app.status, app.created_at,
          COUNT(*) OVER (PARTITION BY app.app_family_id) AS version_count,
          ROW_NUMBER() OVER (PARTITION BY app.app_family_id ORDER BY app.created_at DESC, app.app_id DESC) AS version_rank
   FROM fused_apps app
   WHERE app.account_id = $1
 )
 SELECT family.app_family_id, family.display_name, family.kind, COALESCE(family.target_language, ''),
 COALESCE(latest.version_count, 0),
 COALESCE(latest.app_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(latest.version, ''),
 COALESCE(latest.status, ''), latest.created_at,
 COALESCE(stable.app_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(stable.version, '')
 FROM fused_app_families family
 LEFT JOIN version_catalog latest ON latest.app_family_id = family.app_family_id
   AND latest.account_id = family.account_id AND latest.version_rank = 1
 LEFT JOIN fused_apps stable ON stable.app_id = family.mcp_stable_app_id
   AND stable.app_family_id = family.app_family_id AND stable.account_id = family.account_id
 `+appFamilyCatalogWhere+`
 ORDER BY family.canonical_name, family.app_family_id
 LIMIT $6 OFFSET $7`, append(args, limit, offset)...)
	// Query failures must never be presented as an empty successful catalogue.
	if err != nil {
		return nil, 0, fmt.Errorf("list app families: %w", err)
	}
	defer rows.Close()
	items := make([]AppFamilyCatalogItem, 0)
	for rows.Next() {
		var item AppFamilyCatalogItem
		// The fixed projection contains only family metadata and the explicit MCP promotion pointer.
		if err := rows.Scan(&item.AppFamilyID, &item.Name, &item.Kind, &item.TargetLanguage, &item.VersionCount,
			&item.LatestAppID, &item.LatestVersion, &item.LatestStatus, &item.LatestCreatedAt,
			&item.StableAppID, &item.StableVersion); err != nil {
			return nil, 0, fmt.Errorf("scan app family: %w", err)
		}
		items = append(items, item)
	}
	// Interrupted reads must not return partial version counts as authoritative results.
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_app_families family`+appFamilyCatalogWhere, args...).Scan(&total)
	return items, total, err
}

// ListAuthorizedAppFamilies forwards actor-specific reads without caching or regrouping version rows.
func (s *cachedStore) ListAuthorizedAppFamilies(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind, search string, limit, offset int) ([]AppFamilyCatalogItem, int, error) {
	repository, ok := s.Store.(AppFamilyCatalogRepository)
	// Missing storage support must fail closed rather than paginate before authorization.
	if !ok {
		return nil, 0, errAppCatalogUnavailable
	}
	return repository.ListAuthorizedAppFamilies(ctx, accountID, scope, kind, search, limit, offset)
}

var _ AppFamilyCatalogRepository = (*postgresStore)(nil)
var _ AppFamilyCatalogRepository = (*cachedStore)(nil)
