package store

import (
	"context"
	"errors"
	"github.com/google/uuid"
)

// scheduleStore requires authoritative persistence rather than caching automation state or plaintext input.
func (s *cachedStore) scheduleStore() (UnifiedAppScheduleStore, error) {
	repository, ok := s.Store.(UnifiedAppScheduleStore)
	// Missing persistence must fail closed before automatic work is admitted.
	if !ok {
		return nil, errors.New("schedule store unavailable")
	}
	return repository, nil
}

// ListUnifiedAppSchedules preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) ListUnifiedAppSchedules(ctx context.Context, account, family uuid.UUID, key []byte) ([]UnifiedAppSchedule, error) {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return nil, err
	}
	return repository.ListUnifiedAppSchedules(ctx, account, family, key)
}

// SaveUnifiedAppSchedule preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) SaveUnifiedAppSchedule(ctx context.Context, item UnifiedAppSchedule, key []byte) error {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return err
	}
	return repository.SaveUnifiedAppSchedule(ctx, item, key)
}

// DeleteUnifiedAppSchedule preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) DeleteUnifiedAppSchedule(ctx context.Context, account, family, id uuid.UUID, revision int64) error {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return err
	}
	return repository.DeleteUnifiedAppSchedule(ctx, account, family, id, revision)
}

// DueUnifiedAppSchedules preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) DueUnifiedAppSchedules(ctx context.Context, key []byte) ([]UnifiedAppSchedule, error) {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return nil, err
	}
	return repository.DueUnifiedAppSchedules(ctx, key)
}

// ReserveScheduledExecution preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) ReserveScheduledExecution(ctx context.Context, item UnifiedAppSchedule, record ExecutionResult) (bool, error) {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return false, err
	}
	return repository.ReserveScheduledExecution(ctx, item, record)
}

// FinishScheduledExecution preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) FinishScheduledExecution(ctx context.Context, id, executionID uuid.UUID, status string) error {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return err
	}
	return repository.FinishScheduledExecution(ctx, id, executionID, status)
}

// RecoverScheduledExecutions preserves database ownership and keeps private schedule input outside caches.
func (s *cachedStore) RecoverScheduledExecutions(ctx context.Context) ([]ExecutionResult, error) {
	repository, err := s.scheduleStore()
	// A cache wrapper cannot substitute for durable schedule state.
	if err != nil {
		return nil, err
	}
	return repository.RecoverScheduledExecutions(ctx)
}
