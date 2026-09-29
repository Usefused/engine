package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// UnifiedAppTargetStore checks whether an exact version currently accepts new traffic.
type UnifiedAppTargetStore interface {
	IsUnifiedAppTrafficTarget(context.Context, uuid.UUID) (bool, error)
}

// IsUnifiedAppTrafficTarget lets PostgreSQL evaluate version readiness and family promotion together.
func (s *postgresStore) IsUnifiedAppTrafficTarget(ctx context.Context, appID uuid.UUID) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM fused_app_families family
			JOIN fused_apps app ON app.app_id = family.unified_active_app_id
			JOIN fused_unified_app_bundles bundle ON bundle.app_id = app.app_id
			WHERE app.app_id = $1 AND app.app_family_id = family.app_family_id
			  AND family.kind = 'unified_app' AND family.archived_at IS NULL
			  AND app.status IN ('active', 'deprecated')
		)
	`, appID).Scan(&active)
	// Database uncertainty may not authorize an older or unready version.
	if err != nil {
		return false, fmt.Errorf("check unified app traffic target: %w", err)
	}
	return active, nil
}

// IsUnifiedAppTrafficTarget preserves the target check through the Engine cache wrapper.
func (s *cachedStore) IsUnifiedAppTrafficTarget(ctx context.Context, appID uuid.UUID) (bool, error) {
	repository, ok := s.Store.(UnifiedAppTargetStore)
	// A wrapper without the authoritative store must not reopen old versions.
	if !ok {
		return false, fmt.Errorf("check unified app traffic target: store unavailable")
	}
	return repository.IsUnifiedAppTrafficTarget(ctx, appID)
}

var _ UnifiedAppTargetStore = (*postgresStore)(nil)
var _ UnifiedAppTargetStore = (*cachedStore)(nil)
