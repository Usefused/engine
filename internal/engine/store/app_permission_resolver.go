package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// AppPermissionType preserves the shared SDK adapter while separating REST API authority.
func AppPermissionType(family *AppFamily) string {
	// Missing identities must never default to a grantable SDK kind.
	if family == nil {
		return ""
	}
	// REST delivery is immutable family identity, not a client-requested permission label.
	if family.Kind == AppKindSDK && family.DeliveryMode == AppDeliveryModeAPI {
		return "api"
	}
	return string(family.Kind)
}

// ResolveAppPermission resolves only account-owned family IDs, including families without live versions.
func (s *postgresStore) ResolveAppPermission(ctx context.Context, accountID, familyID uuid.UUID, action accesscontrol.Permission) (accesscontrol.Permission, error) {
	var appType string
	err := s.db.QueryRow(ctx, `SELECT CASE WHEN kind = 'sdk' AND delivery_mode = 'api' THEN 'api' ELSE kind END
		FROM fused_app_families WHERE account_id = $1 AND app_family_id = $2`, accountID, familyID).Scan(&appType)
	// Opaque missing or foreign identities expose neither type nor authority.
	if err != nil {
		return "", accesscontrol.ErrPolicyDenied
	}
	permission := accesscontrol.AppPermission(appType, action)
	return permission, accesscontrol.ValidatePermission(permission)
}

// ResolveAppPermissionScope expands typed grants in one SQL query before collection pagination or counting.
func (s *postgresStore) ResolveAppPermissionScope(ctx context.Context, accountID uuid.UUID, grants []accesscontrol.Grant, action accesscontrol.Permission) (accesscontrol.AuthorizedScope, error) {
	permissions, resourceTypes, resourceIDs := make([]string, 0), make([]string, 0), make([]uuid.UUID, 0)
	for _, grant := range grants {
		// Only exact action suffixes participate; manage never implies read.
		if !strings.HasPrefix(string(grant.Permission), "app.") || !strings.HasSuffix(string(grant.Permission), "."+strings.TrimPrefix(string(action), "app.")) {
			continue
		}
		permissions = append(permissions, string(grant.Permission))
		resourceTypes = append(resourceTypes, string(grant.Resource.Type))
		resourceIDs = append(resourceIDs, grant.Resource.ID)
	}
	// Empty grant sets must not become an unrestricted catalogue query.
	if len(permissions) == 0 {
		return accesscontrol.AuthorizedScope{}, nil
	}
	rows, err := s.db.Query(ctx, `SELECT family.app_family_id FROM fused_app_families family
		WHERE family.account_id = $1 AND EXISTS (
		 SELECT 1 FROM unnest($2::text[], $3::text[], $4::uuid[]) AS grant_row(permission, resource_type, resource_id)
		 WHERE grant_row.permission = 'app.' || CASE WHEN family.kind = 'sdk' AND family.delivery_mode = 'api' THEN 'api' ELSE family.kind END || '.' || $5
		 AND (grant_row.resource_type = 'workspace' OR (grant_row.resource_type = 'app' AND grant_row.resource_id = family.app_family_id))
		) ORDER BY family.app_family_id`, accountID, permissions, resourceTypes, resourceIDs, strings.TrimPrefix(string(action), "app."))
	// Storage errors fail closed rather than dropping the type filter.
	if err != nil {
		return accesscontrol.AuthorizedScope{}, fmt.Errorf("resolve app permission scope: %w", err)
	}
	defer rows.Close()
	scope := accesscontrol.AuthorizedScope{}
	for rows.Next() {
		var id uuid.UUID
		// An incomplete identity projection cannot authorize a partial collection.
		if err := rows.Scan(&id); err != nil {
			return accesscontrol.AuthorizedScope{}, err
		}
		scope.IDs = append(scope.IDs, id)
	}
	return scope, rows.Err()
}
