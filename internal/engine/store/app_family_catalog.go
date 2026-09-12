package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// AppFamilyCatalogItem describes one logical application without selecting an implicit version.
type AppFamilyCatalogItem struct {
	AppFamilyID    uuid.UUID
	Name           string
	Kind           AppKind
	TargetLanguage string
	VersionCount   int
	StableAppID    uuid.UUID
	StableVersion  string
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
	rows, err := s.db.Query(ctx, `
 SELECT family.app_family_id, family.display_name, family.kind, COALESCE(family.target_language, ''),
 COUNT(app.app_id), COALESCE(stable.app_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(stable.version, '')
 FROM fused_app_families family
 LEFT JOIN fused_apps app ON app.app_family_id = family.app_family_id AND app.account_id = family.account_id
 LEFT JOIN fused_apps stable ON stable.app_id = family.mcp_stable_app_id
   AND stable.app_family_id = family.app_family_id AND stable.account_id = family.account_id
 `+appFamilyCatalogWhere+`
 GROUP BY family.app_family_id, stable.app_id, stable.version
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
		if err := rows.Scan(&item.AppFamilyID, &item.Name, &item.Kind, &item.TargetLanguage, &item.VersionCount, &item.StableAppID, &item.StableVersion); err != nil {
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
