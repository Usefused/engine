package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OwnedServiceRecoveryStore keeps membership filtering in SQL and preserves local pins.
type OwnedServiceRecoveryStore interface {
	MissingOwnedServiceIDs(context.Context, []uuid.UUID) ([]uuid.UUID, error)
	UpsertServiceContractSnapshot(context.Context, ServiceContractSnapshot) (*ServiceContractSnapshot, error)
}

const missingOwnedServiceIDsSQL = `
SELECT requested.id FROM unnest($1::uuid[]) AS requested(id)
WHERE NOT EXISTS (
    SELECT 1 FROM fused_workspace_services enabled WHERE enabled.service_id = requested.id
)`

// MissingOwnedServiceIDs uses one anti-join rather than one membership query per owned service.
func (s *postgresStore) MissingOwnedServiceIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	// Empty discovery needs no database access and must not become an unscoped lookup.
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, missingOwnedServiceIDsSQL, ids)
	// A failed lookup is not proof that any service is absent.
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// MissingOwnedServiceIDs deliberately bypasses caches so startup preserves every committed pin.
func (s *cachedStore) MissingOwnedServiceIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	delegate, ok := s.Store.(OwnedServiceRecoveryStore)
	// Recovery must not silently fall back to an N+1 or unfiltered membership path.
	if !ok {
		return nil, errors.New("owned service recovery store unavailable")
	}
	return delegate.MissingOwnedServiceIDs(ctx, ids)
}
