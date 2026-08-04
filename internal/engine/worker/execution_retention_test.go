package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retentionStoreStub struct {
	results []int64
	err     error
	calls   int
	limits  []int
	before  []time.Time
}

func (s *retentionStoreStub) DeleteEngineExecutionEventsBefore(_ context.Context, before time.Time, limit int) (int64, error) {
	s.calls++
	s.limits = append(s.limits, limit)
	s.before = append(s.before, before)
	if s.err != nil {
		return 0, s.err
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func TestCleanupExpiredExecutionEventsDrainsBoundedBatches(t *testing.T) {
	store := &retentionStoreStub{results: []int64{100, 100, 4}}
	before := time.Now().UTC().Add(-30 * 24 * time.Hour)

	cleanupExpiredExecutionEvents(context.Background(), store, before, 100)

	if store.calls != 3 {
		t.Fatalf("delete calls = %d, want 3", store.calls)
	}
	for index := range store.limits {
		if store.limits[index] != 100 || !store.before[index].Equal(before) {
			t.Fatalf("call %d = limit %d before %s", index, store.limits[index], store.before[index])
		}
	}
}

func TestCleanupExpiredExecutionEventsStopsAfterStoreError(t *testing.T) {
	store := &retentionStoreStub{err: errors.New("database unavailable")}

	cleanupExpiredExecutionEvents(context.Background(), store, time.Now(), 50)

	if store.calls != 1 {
		t.Fatalf("delete calls = %d, want 1", store.calls)
	}
}

func TestStartExecutionRetentionWorkerRejectsDisabledConfiguration(t *testing.T) {
	store := &retentionStoreStub{}
	if worker := StartExecutionRetentionWorker(context.Background(), store, 0, 100); worker != nil {
		t.Fatal("worker started with retention disabled")
	}
	if worker := StartExecutionRetentionWorker(context.Background(), store, 30, 0); worker != nil {
		t.Fatal("worker started with invalid batch size")
	}
}
