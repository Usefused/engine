package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ExecutionAppTargetStore checks whether an exact version currently accepts new traffic.
type ExecutionAppTargetStore interface {
	IsExecutionAppTrafficTarget(context.Context, uuid.UUID) (bool, error)
}

// IsExecutionAppTrafficTarget lets PostgreSQL evaluate version readiness and family promotion together.
func (s *postgresStore) IsExecutionAppTrafficTarget(ctx context.Context, appID uuid.UUID) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM fused_app_families family
			JOIN fused_apps app ON app.app_id = family.execution_active_app_id
			JOIN fused_execution_app_bundles bundle ON bundle.app_id = app.app_id
			WHERE app.app_id = $1 AND app.app_family_id = family.app_family_id
			  AND family.kind = 'execution' AND family.archived_at IS NULL
			  AND app.status IN ('active', 'deprecated')
		)
	`, appID).Scan(&active)
	// Database uncertainty may not authorize an older or unready version.
	if err != nil {
		return false, fmt.Errorf("check execution app traffic target: %w", err)
	}
	return active, nil
}

// IsExecutionAppTrafficTarget preserves the target check through the Engine cache wrapper.
func (s *cachedStore) IsExecutionAppTrafficTarget(ctx context.Context, appID uuid.UUID) (bool, error) {
	repository, ok := s.Store.(ExecutionAppTargetStore)
	// A wrapper without the authoritative store must not reopen old versions.
	if !ok {
		return false, fmt.Errorf("check execution app traffic target: store unavailable")
	}
	return repository.IsExecutionAppTrafficTarget(ctx, appID)
}

var _ ExecutionAppTargetStore = (*postgresStore)(nil)
var _ ExecutionAppTargetStore = (*cachedStore)(nil)
