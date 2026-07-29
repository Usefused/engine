package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type captureUsageCounterStore struct {
	mu      sync.Mutex
	batches [][]models.EngineUsageIncrement
}

func (s *captureUsageCounterStore) IncrementRuntimeUsageCounters(_ context.Context, increments []models.EngineUsageIncrement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]models.EngineUsageIncrement(nil), increments...))
	return nil
}

func (s *captureUsageCounterStore) firstBatchLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return 0
	}
	return len(s.batches[0])
}

func withWorkerFlushTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := workerFlushTimeout
	workerFlushTimeout = timeout
	t.Cleanup(func() {
		workerFlushTimeout = previous
	})
}

func TestUsageCounterWorkerFlushesByBatchSize(t *testing.T) {
	store := &captureUsageCounterStore{}
	worker := NewUsageCounterWorker(store, UsageCounterOptions{QueueSize: 4, BatchSize: 2, FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop(context.Background())

	worker.Record(testUsageIncrement(models.EngineUsageMetricExecutionTotal))
	worker.Record(testUsageIncrement(models.EngineUsageMetricExecutionSuccess))

	deadline := time.After(2 * time.Second)
	for store.firstBatchLen() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected usage counter batch to flush by size")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got := store.firstBatchLen(); got != 2 {
		t.Fatalf("batch size = %d, want 2", got)
	}
}

type blockingUsageCounterStore struct {
	started chan struct{}
	once    sync.Once
}

func (s *blockingUsageCounterStore) IncrementRuntimeUsageCounters(ctx context.Context, increments []models.EngineUsageIncrement) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

func TestUsageCounterWorkerStopBoundsFinalFlush(t *testing.T) {
	withWorkerFlushTimeout(t, 20*time.Millisecond)
	store := &blockingUsageCounterStore{started: make(chan struct{})}
	worker := NewUsageCounterWorker(store, UsageCounterOptions{QueueSize: 4, BatchSize: 10, FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	worker.Record(testUsageIncrement(models.EngineUsageMetricExecutionTotal))

	started := make(chan struct{})
	go func() {
		defer close(started)
		worker.Stop(context.Background())
	}()

	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected worker stop to respect bounded final flush timeout")
	}
}

type captureUsageReportStore struct {
	reports []models.EngineUsageReport
	flushed []uuid.UUID
}

func (s *captureUsageReportStore) ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error) {
	pending := make([]models.EngineUsageReport, 0, len(s.reports))
	for _, report := range s.reports {
		if !uuidInSlice(report.ReportID, s.flushed) {
			pending = append(pending, report)
		}
	}
	if len(pending) > limit {
		return pending[:limit], nil
	}
	return pending, nil
}

func (s *captureUsageReportStore) MarkRuntimeUsageReportsFlushed(ctx context.Context, reportIDs []uuid.UUID, flushedAt time.Time) error {
	s.flushed = append(s.flushed, reportIDs...)
	return nil
}

type captureUsageReportClient struct {
	sent []models.EngineUsageReport
}

func (c *captureUsageReportClient) SendUsageReports(ctx context.Context, engineVersion, engineBuildHash string, reports []models.EngineUsageReport, reportedAt time.Time) error {
	c.sent = append(c.sent, reports...)
	return nil
}

func TestUsageReportFlushWorkerSendsAndMarksAcceptedReports(t *testing.T) {
	reportID := uuid.New()
	store := &captureUsageReportStore{reports: []models.EngineUsageReport{{
		ReportID:      reportID,
		Metric:        models.EngineUsageMetricExecutionTotal,
		BucketStart:   time.Now().UTC().Truncate(time.Minute),
		BucketSeconds: 60,
		Count:         3,
	}}}
	client := &captureUsageReportClient{}
	worker := NewUsageReportFlushWorker(store, client, UsageReportFlushOptions{Interval: time.Hour, BatchLimit: 10})

	ctx, cancel := context.WithCancel(context.Background())
	worker.Start(ctx)
	cancel()
	worker.Stop(context.Background())

	if len(client.sent) != 1 || client.sent[0].ReportID != reportID {
		t.Fatalf("sent reports = %#v", client.sent)
	}
	if len(store.flushed) != 1 || store.flushed[0] != reportID {
		t.Fatalf("flushed ids = %#v", store.flushed)
	}
}

type blockingUsageReportStore struct {
	started chan struct{}
	once    sync.Once
}

func (s *blockingUsageReportStore) ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *blockingUsageReportStore) MarkRuntimeUsageReportsFlushed(context.Context, []uuid.UUID, time.Time) error {
	return errors.New("unexpected mark flushed")
}

func TestUsageReportFlushWorkerStopBoundsBlockedFlush(t *testing.T) {
	withWorkerFlushTimeout(t, 20*time.Millisecond)
	store := &blockingUsageReportStore{started: make(chan struct{})}
	client := &captureUsageReportClient{}
	worker := NewUsageReportFlushWorker(store, client, UsageReportFlushOptions{Interval: time.Hour, BatchLimit: 10})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	select {
	case <-store.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected usage report flush to start")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Stop(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected usage report worker stop to respect bounded flush timeout")
	}
}

func testUsageIncrement(metric string) models.EngineUsageIncrement {
	return models.EngineUsageIncrement{
		Metric:        metric,
		BucketStart:   time.Now().UTC().Truncate(time.Minute),
		BucketSeconds: 60,
		Count:         1,
	}
}

func uuidInSlice(id uuid.UUID, ids []uuid.UUID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
