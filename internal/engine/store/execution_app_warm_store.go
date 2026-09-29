package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// WarmUnifiedAppBundle names the one traffic target and immutable source for a family.
type WarmUnifiedAppBundle struct {
	FamilyID uuid.UUID
	AppID    uuid.UUID
	BundleJS string
}

// UnifiedAppWarmStore lists only ready traffic targets for Engine startup and plan refresh.
type UnifiedAppWarmStore interface {
	ListWarmUnifiedAppBundles(context.Context) ([]WarmUnifiedAppBundle, error)
}

// ListWarmUnifiedAppBundles reads every eligible bundle in one bounded, set-based query.
func (s *postgresStore) ListWarmUnifiedAppBundles(ctx context.Context) ([]WarmUnifiedAppBundle, error) {
	rows, err := s.db.Query(ctx, `
		SELECT family.app_family_id, app.app_id, bundle.bundle_js, app.bundle_digest
		FROM fused_app_families family
		JOIN fused_apps app ON app.app_id = family.unified_active_app_id
		JOIN fused_unified_app_bundles bundle ON bundle.app_id = app.app_id
		WHERE family.kind = 'unified_app' AND family.archived_at IS NULL
		  AND app.status IN ('active', 'deprecated')
	`)
	// A failed scan cannot authorize a stale in-memory traffic target.
	if err != nil {
		return nil, fmt.Errorf("list warm unified apps: %w", err)
	}
	defer rows.Close()
	bundles := make([]WarmUnifiedAppBundle, 0)
	// SQL selects only current traffic targets; iteration validates their immutable bytes without filtering app versions in Go.
	for rows.Next() {
		var bundle WarmUnifiedAppBundle
		var digest string
		if err := rows.Scan(&bundle.FamilyID, &bundle.AppID, &bundle.BundleJS, &digest); err != nil {
			return nil, fmt.Errorf("scan warm unified app: %w", err)
		}
		// Persisted code must still match the exact bundle that won traffic promotion.
		if UnifiedAppBundleDigest([]byte(bundle.BundleJS)) != digest {
			return nil, ErrUnifiedAppBundleDigestMismatch
		}
		bundles = append(bundles, bundle)
	}
	// Streaming errors invalidate the whole warm snapshot so partial results cannot preserve a stale target.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list warm unified apps: %w", err)
	}
	return bundles, nil
}

// ListWarmUnifiedAppBundles keeps the cache wrapper on the same authoritative SQL path.
func (s *cachedStore) ListWarmUnifiedAppBundles(ctx context.Context) ([]WarmUnifiedAppBundle, error) {
	repository, ok := s.Store.(UnifiedAppWarmStore)
	// A wrapper without the backing query cannot guess which version is current.
	if !ok {
		return nil, errors.New("warm unified app store unavailable")
	}
	return repository.ListWarmUnifiedAppBundles(ctx)
}

var _ UnifiedAppWarmStore = (*postgresStore)(nil)
var _ UnifiedAppWarmStore = (*cachedStore)(nil)
