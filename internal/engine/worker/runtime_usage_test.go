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

// withWorkerFlushTimeout bounds shutdown-path tests without changing production defaults.
func withWorkerFlushTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := workerFlushTimeout
	workerFlushTimeout = timeout
	t.Cleanup(func() {
		workerFlushTimeout = previous
	})
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

// uuidInSlice checks whether the flush acknowledgement contains one exact report identity.
func uuidInSlice(id uuid.UUID, ids []uuid.UUID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
