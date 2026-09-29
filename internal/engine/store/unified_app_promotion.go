package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrUnifiedAppTrafficChanged = errors.New("Unified App traffic changed; refresh and try again")

// UnifiedAppPromotionStore exposes a compare-and-swap over the existing deployment pointer.
type UnifiedAppPromotionStore interface {
	UnifiedAppTrafficTarget(context.Context, uuid.UUID) (uuid.UUID, error)
	PromoteUnifiedAppVersion(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
}

// UnifiedAppTrafficTarget reads deployment state without inferring it from version order or status.
func (s *postgresStore) UnifiedAppTrafficTarget(ctx context.Context, familyID uuid.UUID) (uuid.UUID, error) {
	var target uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT COALESCE(unified_active_app_id, '00000000-0000-0000-0000-000000000000'::uuid) FROM fused_app_families WHERE app_family_id=$1 AND kind='unified_app' AND archived_at IS NULL`, familyID).Scan(&target)
	// Missing and archived families cannot advertise a traffic destination.
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAppFamilyNotFound
	}
	return target, err
}

// PromoteUnifiedAppVersion atomically restores a retained bundle without editing its immutable configuration.
func (s *postgresStore) PromoteUnifiedAppVersion(ctx context.Context, familyID, appID, expected uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	// Locking and readiness checks must share the same transaction as promotion.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current uuid.UUID
	err = tx.QueryRow(ctx, `SELECT COALESCE(unified_active_app_id, '00000000-0000-0000-0000-000000000000'::uuid) FROM fused_app_families WHERE app_family_id=$1 AND kind='unified_app' AND archived_at IS NULL FOR UPDATE`, familyID).Scan(&current)
	// A removed family cannot be revived by a stale control request.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppFamilyNotFound
	}
	// Database errors must not be interpreted as an empty deployment.
	if err != nil {
		return err
	}
	// Repeating the same promotion is safe; a competing destination requires a fresh review.
	if current != expected && current != appID {
		return ErrUnifiedAppTrafficChanged
	}
	var script, digest string
	err = tx.QueryRow(ctx, `SELECT bundle.bundle_js, app.bundle_digest FROM fused_apps app JOIN fused_unified_app_bundles bundle ON bundle.app_id=app.app_id WHERE app.app_family_id=$1 AND app.app_id=$2 AND app.status IN ('active','deprecated') AND app.source_hash=bundle.source_hash`, familyID, appID).Scan(&script, &digest)
	// Deleted, unready and cross-family versions are never valid promotion targets.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnifiedAppBundleNotFound
	}
	// Storage failures cannot authorize promotion.
	if err != nil {
		return err
	}
	// Verify retained code before changing the serving pointer, including idempotent retries.
	if UnifiedAppBundleDigest([]byte(script)) != digest {
		return ErrUnifiedAppBundleDigestMismatch
	}
	// Reuse deployment's pointer update and retirement of sessions pinned to the previous version.
	if err := promoteUnifiedAppVersionTx(ctx, tx, familyID, appID); err != nil {
		return err
	}
	// A commit failure has an unknown outcome and cannot be reported as convergence.
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Record idempotency through the existing control audit, without a competing event stream.
	if current == appID {
		accesscontrol.MarkMutationAuditUnchanged(ctx)
	}
	return nil
}

// UnifiedAppTrafficTarget bypasses cache so UI and CLI review the current deployment.
func (s *cachedStore) UnifiedAppTrafficTarget(ctx context.Context, familyID uuid.UUID) (uuid.UUID, error) {
	repository, ok := s.Store.(UnifiedAppPromotionStore)
	// Missing support is an availability error, never an empty traffic target.
	if !ok {
		return uuid.Nil, fmt.Errorf("Unified App promotion store unavailable")
	}
	return repository.UnifiedAppTrafficTarget(ctx, familyID)
}

// PromoteUnifiedAppVersion delegates the atomic transition through the cache wrapper.
func (s *cachedStore) PromoteUnifiedAppVersion(ctx context.Context, familyID, appID, expected uuid.UUID) error {
	repository, ok := s.Store.(UnifiedAppPromotionStore)
	// No local fallback may simulate a successful deployment change.
	if !ok {
		return fmt.Errorf("Unified App promotion store unavailable")
	}
	return repository.PromoteUnifiedAppVersion(ctx, familyID, appID, expected)
}
