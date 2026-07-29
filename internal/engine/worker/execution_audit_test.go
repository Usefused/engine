package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type captureExecutionAuditStore struct {
	mu      sync.Mutex
	batches [][]models.EngineExecutionEvent
}

func (s *captureExecutionAuditStore) BatchCreateEngineExecutionEvents(_ context.Context, events []models.EngineExecutionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := append([]models.EngineExecutionEvent(nil), events...)
	s.batches = append(s.batches, copied)
	return nil
}

func (s *captureExecutionAuditStore) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *captureExecutionAuditStore) firstBatchLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return 0
	}
	return len(s.batches[0])
}

func TestExecutionAuditWorkerFlushesByBatchSize(t *testing.T) {
	store := &captureExecutionAuditStore{}
	worker := NewExecutionAuditWorker(store, ExecutionAuditOptions{
		QueueSize:     4,
		BatchSize:     2,
		FlushInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop(context.Background())

	worker.Record(testExecutionEvent("list_repos"))
	worker.Record(testExecutionEvent("get_repo"))

	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected execution audit batch to flush by size")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := store.firstBatchLen(); got != 2 {
		t.Fatalf("batch size = %d, want 2", got)
	}
}

func TestExecutionAuditWorkerStopFlushesPendingEvents(t *testing.T) {
	store := &captureExecutionAuditStore{}
	worker := NewExecutionAuditWorker(store, ExecutionAuditOptions{
		QueueSize:     4,
		BatchSize:     10,
		FlushInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	worker.Record(testExecutionEvent("list_repos"))
	worker.Stop(context.Background())

	if got := store.firstBatchLen(); got != 1 {
		t.Fatalf("pending batch size after stop = %d, want 1", got)
	}
}

type blockingExecutionAuditStore struct {
	started chan struct{}
	once    sync.Once
}

func (s *blockingExecutionAuditStore) BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

func TestExecutionAuditWorkerStopBoundsFinalFlush(t *testing.T) {
	withWorkerFlushTimeout(t, 20*time.Millisecond)
	store := &blockingExecutionAuditStore{started: make(chan struct{})}
	worker := NewExecutionAuditWorker(store, ExecutionAuditOptions{
		QueueSize:     4,
		BatchSize:     10,
		FlushInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	worker.Record(testExecutionEvent("list_repos"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Stop(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected audit worker stop to respect bounded final flush timeout")
	}
}

func testExecutionEvent(endpoint string) models.EngineExecutionEvent {
	now := time.Now()
	return models.EngineExecutionEvent{
		ID:           uuid.New(),
		ArtifactID:   uuid.New(),
		Transport:    models.EngineExecutionTransportSDK,
		EndpointName: endpoint,
		Status:       models.EngineExecutionStatusSuccess,
		StartedAt:    now,
		EndedAt:      now,
		LatencyMs:    1,
		CreatedAt:    now,
	}
}
