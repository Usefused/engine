package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FamilyExecutionResultStore reads history without requiring callers to track version IDs.
type FamilyExecutionResultStore interface {
	GetFamilyExecutionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*ExecutionResult, error)
}

// GetFamilyExecutionResult retains exact receipt identity while bounding access by tenant and family.
func (s *postgresStore) GetFamilyExecutionResult(ctx context.Context, accountID, familyID, executionID uuid.UUID) (*ExecutionResult, error) {
	row := s.db.QueryRow(ctx, `SELECT `+executionResultColumns+` FROM fused_unified_app_results
		WHERE id=$1 AND account_id=$2 AND app_family_id=$3 AND (expires_at IS NULL OR expires_at>NOW())`, executionID, accountID, familyID)
	record, err := scanExecutionResult(row)
	// Expired and cross-family records remain indistinguishable from unknown IDs.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionResultNotFound
	}
	return record, err
}

// GetFamilyExecutionResult bypasses caches so retention and completion remain authoritative.
func (s *cachedStore) GetFamilyExecutionResult(ctx context.Context, accountID, familyID, executionID uuid.UUID) (*ExecutionResult, error) {
	repository, ok := s.Store.(FamilyExecutionResultStore)
	// Unsupported storage must fail closed rather than broaden the read scope.
	if !ok {
		return nil, fmt.Errorf("family execution result store unavailable")
	}
	return repository.GetFamilyExecutionResult(ctx, accountID, familyID, executionID)
}
