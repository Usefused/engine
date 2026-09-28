package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// WarmExecutionAppBundle names the one traffic target and immutable source for a family.
type WarmExecutionAppBundle struct {
	FamilyID uuid.UUID
	AppID    uuid.UUID
	BundleJS string
}

// ExecutionAppWarmStore lists only ready traffic targets for Engine startup and plan refresh.
type ExecutionAppWarmStore interface {
	ListWarmExecutionAppBundles(context.Context) ([]WarmExecutionAppBundle, error)
}

// ListWarmExecutionAppBundles reads every eligible bundle in one bounded, set-based query.
func (s *postgresStore) ListWarmExecutionAppBundles(ctx context.Context) ([]WarmExecutionAppBundle, error) {
	rows, err := s.db.Query(ctx, `
		SELECT family.app_family_id, app.app_id, bundle.bundle_js, app.bundle_digest
		FROM fused_app_families family
		JOIN fused_apps app ON app.app_id = family.execution_active_app_id
		JOIN fused_execution_app_bundles bundle ON bundle.app_id = app.app_id
		WHERE family.kind = 'execution' AND family.archived_at IS NULL
		  AND app.status IN ('active', 'deprecated')
	`)
	// A failed scan cannot authorize a stale in-memory traffic target.
	if err != nil {
		return nil, fmt.Errorf("list warm execution apps: %w", err)
	}
	defer rows.Close()
	bundles := make([]WarmExecutionAppBundle, 0)
	// SQL selects only current traffic targets; iteration validates their immutable bytes without filtering app versions in Go.
	for rows.Next() {
		var bundle WarmExecutionAppBundle
		var digest string
		if err := rows.Scan(&bundle.FamilyID, &bundle.AppID, &bundle.BundleJS, &digest); err != nil {
			return nil, fmt.Errorf("scan warm execution app: %w", err)
		}
		// Persisted code must still match the exact bundle that won traffic promotion.
		if ExecutionAppBundleDigest([]byte(bundle.BundleJS)) != digest {
			return nil, ErrExecutionAppBundleDigestMismatch
		}
		bundles = append(bundles, bundle)
	}
	// Streaming errors invalidate the whole warm snapshot so partial results cannot preserve a stale target.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list warm execution apps: %w", err)
	}
	return bundles, nil
}

// ListWarmExecutionAppBundles keeps the cache wrapper on the same authoritative SQL path.
func (s *cachedStore) ListWarmExecutionAppBundles(ctx context.Context) ([]WarmExecutionAppBundle, error) {
	repository, ok := s.Store.(ExecutionAppWarmStore)
	// A wrapper without the backing query cannot guess which version is current.
	if !ok {
		return nil, errors.New("warm execution app store unavailable")
	}
	return repository.ListWarmExecutionAppBundles(ctx)
}

var _ ExecutionAppWarmStore = (*postgresStore)(nil)
var _ ExecutionAppWarmStore = (*cachedStore)(nil)
