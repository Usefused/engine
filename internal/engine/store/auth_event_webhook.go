package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// AuthEventAppFamily is the minimal credential-free identity required to project one internal auth event safely.
type AuthEventAppFamily struct {
	AppID       uuid.UUID
	AppFamilyID uuid.UUID
	AccountID   uuid.UUID
	Kind        AppKind
}

// AuthEventAppFamilyResolver resolves current and tombstoned app versions in one bounded query.
type AuthEventAppFamilyResolver interface {
	ResolveAuthEventAppFamilies(context.Context, []uuid.UUID) (map[uuid.UUID]AuthEventAppFamily, error)
}

// ResolveAuthEventAppFamilies maps event provenance to its stable family without requiring the originating version to remain active.
func (s *postgresStore) ResolveAuthEventAppFamilies(ctx context.Context, appIDs []uuid.UUID) (map[uuid.UUID]AuthEventAppFamily, error) {
	resolved := make(map[uuid.UUID]AuthEventAppFamily, len(appIDs))
	// Empty batches are valid worker polls and require no database round trip.
	if len(appIDs) == 0 {
		return resolved, nil
	}
	rows, err := s.db.Query(ctx, resolveAuthEventAppFamiliesSQL, appIDs)
	// Database uncertainty must not be converted into dropped lifecycle events.
	if err != nil {
		return nil, fmt.Errorf("resolve auth event app families: %w", err)
	}
	defer rows.Close()
	// Streaming rows bounds memory to the requested app-ID batch and avoids per-app lookups.
	for rows.Next() {
		var identity AuthEventAppFamily
		// The query returns one stable identity for either the live row or its irreversible tombstone.
		if err := rows.Scan(&identity.AppID, &identity.AppFamilyID, &identity.AccountID, &identity.Kind); err != nil {
			return nil, fmt.Errorf("scan auth event app family: %w", err)
		}
		resolved[identity.AppID] = identity
	}
	return resolved, rows.Err()
}

const resolveAuthEventAppFamiliesSQL = `
	SELECT identity.app_id, identity.app_family_id, identity.account_id, family.kind
	FROM (
		SELECT app_id, app_family_id, account_id FROM fused_apps WHERE app_id = ANY($1::uuid[])
		UNION ALL
		SELECT app_id, app_family_id, account_id FROM fused_app_tombstones WHERE app_id = ANY($1::uuid[])
	) identity
	JOIN fused_app_families family
	  ON family.app_family_id = identity.app_family_id
	 AND family.account_id = identity.account_id
`

// ResolveAuthEventAppFamilies forwards the set-based projection through the cache wrapper without creating a second cache contract.
func (s *cachedStore) ResolveAuthEventAppFamilies(ctx context.Context, appIDs []uuid.UUID) (map[uuid.UUID]AuthEventAppFamily, error) {
	resolver, ok := s.Store.(AuthEventAppFamilyResolver)
	// Worker startup validates this capability, while this guard keeps direct callers fail-closed.
	if !ok {
		return nil, errors.New("store does not support auth event app-family resolution")
	}
	return resolver.ResolveAuthEventAppFamilies(ctx, appIDs)
}

var _ AuthEventAppFamilyResolver = (*postgresStore)(nil)
var _ AuthEventAppFamilyResolver = (*cachedStore)(nil)
