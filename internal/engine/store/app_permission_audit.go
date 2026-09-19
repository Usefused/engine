package store

import (
	"context"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// appPermissionForAudit reads the same locked family identity used by the enclosing mutation.
func appPermissionForAudit(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, action accesscontrol.Permission) (accesscontrol.Permission, error) {
	var appType string
	err := tx.QueryRow(ctx, `SELECT CASE WHEN kind = 'sdk' AND delivery_mode = 'api' THEN 'api' ELSE kind END
		FROM fused_app_families WHERE app_family_id = $1`, familyID).Scan(&appType)
	// Missing type evidence must prevent a misleading generic audit event from committing.
	if err != nil {
		return "", err
	}
	permission := accesscontrol.AppPermission(appType, action)
	return permission, accesscontrol.ValidatePermission(permission)
}
