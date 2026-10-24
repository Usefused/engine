package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
)

// recoveryStore requires authoritative storage for every attempt and snapshot operation.
func (s *cachedStore) recoveryStore() (ExecutionRecoveryStore, error) {
	repository, ok := s.Store.(ExecutionRecoveryStore)
	// Cache absence cannot supply process-local durability or ownership.
	if !ok {
		return nil, errors.New("execution recovery store unavailable")
	}
	return repository, nil
}

// GetExecutionRecovery delegates ownership and private snapshot data without caching them.
func (s *cachedStore) GetExecutionRecovery(ctx context.Context, account, app, id uuid.UUID) (*ExecutionRecovery, error) {
	repository, err := s.recoveryStore()
	// Missing persistence must fail closed before recovery work can proceed.
	if err != nil {
		return nil, err
	}
	return repository.GetExecutionRecovery(ctx, account, app, id)
}

// ReadExecutionSnapshot delegates ownership and private snapshot data without caching them.
func (s *cachedStore) ReadExecutionSnapshot(ctx context.Context, account, app, id uuid.UUID, key []byte) (ExecutionSnapshot, error) {
	repository, err := s.recoveryStore()
	// Missing persistence must fail closed before recovery work can proceed.
	if err != nil {
		return ExecutionSnapshot{}, err
	}
	return repository.ReadExecutionSnapshot(ctx, account, app, id, key)
}

// SaveExecutionSnapshot delegates ownership and private snapshot data without caching them.
func (s *cachedStore) SaveExecutionSnapshot(ctx context.Context, account, app, id uuid.UUID, revision int64, state json.RawMessage, key []byte) (int64, error) {
	repository, err := s.recoveryStore()
	// Missing persistence must fail closed before recovery work can proceed.
	if err != nil {
		return 0, err
	}
	return repository.SaveExecutionSnapshot(ctx, account, app, id, revision, state, key)
}

// CheckExecutionAttempt delegates ownership and private snapshot data without caching them.
func (s *cachedStore) CheckExecutionAttempt(ctx context.Context, account, app, id uuid.UUID) error {
	repository, err := s.recoveryStore()
	// Missing persistence must fail closed before recovery work can proceed.
	if err != nil {
		return err
	}
	return repository.CheckExecutionAttempt(ctx, account, app, id)
}

// ReserveExecutionAttempt delegates ownership and private snapshot data without caching them.
func (s *cachedStore) ReserveExecutionAttempt(ctx context.Context, request ExecutionRerunRequest) (*ExecutionResult, bool, error) {
	repository, err := s.recoveryStore()
	// Missing persistence must fail closed before recovery work can proceed.
	if err != nil {
		return nil, false, err
	}
	return repository.ReserveExecutionAttempt(ctx, request)
}

// DueExecutionRecoveries delegates ownership and private snapshot data without caching them.
func (s *cachedStore) DueExecutionRecoveries(ctx context.Context) ([]ExecutionResult, error) {
	repository, err := s.recoveryStore()
	// Missing persistence must fail closed before recovery work can proceed.
	if err != nil {
		return nil, err
	}
	return repository.DueExecutionRecoveries(ctx)
}

// ListExecutionAttempts reads authoritative ownership and never caches attempt state.
func (s *cachedStore) ListExecutionAttempts(ctx context.Context, account, app, source uuid.UUID) ([]ExecutionAttemptSummary, error) {
	repository, err := s.recoveryStore()
	// Missing storage cannot be replaced with stale in-memory history.
	if err != nil {
		return nil, err
	}
	return repository.ListExecutionAttempts(ctx, account, app, source)
}

// AuthorizeExecutionRecovery reloads live grants on every automatic attempt without caching credentials.
func (s *cachedStore) AuthorizeExecutionRecovery(ctx context.Context, account, app, id uuid.UUID) (*AuthProjection, error) {
	repository, err := s.recoveryStore()
	// A failed authority read must prevent automatic work.
	if err != nil {
		return nil, err
	}
	return repository.AuthorizeExecutionRecovery(ctx, account, app, id)
}

// ClaimQueuedExecutionAttempt preserves cross-replica ownership in the underlying database.
func (s *cachedStore) ClaimQueuedExecutionAttempt(ctx context.Context, account, app, id uuid.UUID) (bool, error) {
	repository, err := s.recoveryStore()
	// Local memory cannot stand in for durable queue ownership.
	if err != nil {
		return false, err
	}
	return repository.ClaimQueuedExecutionAttempt(ctx, account, app, id)
}
