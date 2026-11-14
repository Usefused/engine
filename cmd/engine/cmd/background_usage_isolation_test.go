package cmd

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/worker"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestUsageReportingSurvivesBlockedInsightDiscovery reproduces a history scan
// holding the shared probe gate while the real usage worker tries to flush.
func TestUsageReportingSurvivesBlockedInsightDiscovery(t *testing.T) {
	for _, registryUnavailable := range []bool{false, true} {
		name := "accepted"
		// An outage must retain the report even when history discovery is also stalled.
		if registryUnavailable {
			name = "registry_unavailable"
		}
		// Each delivery outcome owns an independent virtual clock and worker lifecycle.
		t.Run(name, func(t *testing.T) {
			// Virtual time exercises the real five-second deadline without slowing the test.
			synctest.Test(t, func(t *testing.T) {
				assertUsageReportingIsolated(t, registryUnavailable)
			})
		})
	}
}

// assertUsageReportingIsolated observes a complete reporting attempt before
// releasing the scan, then drains both goroutines before checking the outcome.
func assertUsageReportingIsolated(t *testing.T, registryUnavailable bool) {
	t.Helper()
	source := &blockedInsightUsageStore{
		entered: make(chan struct{}),
		report: models.EngineUsageReport{
			ReportID: uuid.New(), Metric: models.EngineUsageMetricExecutionTotal,
			BucketStart: time.Now().UTC().Truncate(time.Minute), BucketSeconds: 60, Count: 3,
		},
	}
	background := newSerializedBackgroundStore(source)
	scanCtx, cancelScan := context.WithCancel(t.Context())
	defer cancelScan()
	scanDone := make(chan error, 1)
	// Run discovery concurrently, as the independent public-insights worker does.
	go func() {
		_, err := background.ListUnprojectedPublicInsightServiceIDs(scanCtx, time.Now(), 500)
		scanDone <- err
	}()
	<-source.entered
	client := &isolatedUsageClient{unavailable: registryUnavailable}
	reporter := worker.NewUsageReportFlushWorker(background, client, worker.UsageReportFlushOptions{Interval: time.Hour})
	reporter.Start(t.Context())
	synctest.Wait()
	// Hold discovery beyond the production flush deadline; synctest advances this instantly.
	time.Sleep(6 * time.Second)
	synctest.Wait()
	sentWhileBlocked, flushedWhileBlocked := client.sent, source.flushed
	cancelScan()
	scanErr := <-scanDone
	reporter.Stop(t.Context())
	// A blocked scan must retain caller cancellation after leaving the shared gate.
	if !errors.Is(scanErr, context.Canceled) {
		t.Fatalf("scan error = %v, want context cancellation", scanErr)
	}
	// The usage worker must reach Registry without waiting for the history scan.
	if len(sentWhileBlocked) != 1 || sentWhileBlocked[0].ReportID != source.report.ReportID {
		t.Fatalf("reports sent while history scan blocked = %v", sentWhileBlocked)
	}
	// Only a Registry acknowledgement authorizes marking the durable report flushed.
	if flushedWhileBlocked == registryUnavailable {
		t.Fatalf("flushed while blocked = %v, Registry unavailable = %v", flushedWhileBlocked, registryUnavailable)
	}
}

type blockedInsightUsageStore struct {
	store.Store
	entered chan struct{}
	report  models.EngineUsageReport
	flushed bool
}

// ListUnprojectedPublicInsightServiceIDs models an arbitrarily slow history
// query without relying on wall-clock sleeps or a production database.
func (s *blockedInsightUsageStore) ListUnprojectedPublicInsightServiceIDs(ctx context.Context, _ time.Time, _ int) ([]uuid.UUID, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// ListPendingRuntimeUsageReports preserves the same pending-until-acknowledged
// behavior as the durable counter outbox.
func (s *blockedInsightUsageStore) ListPendingRuntimeUsageReports(context.Context, int) ([]models.EngineUsageReport, error) {
	// A successful flush must not deliver the same report again during shutdown.
	if s.flushed {
		return nil, nil
	}
	return []models.EngineUsageReport{s.report}, nil
}

// MarkRuntimeUsageReportsFlushed accepts only the exact report sent by this test.
func (s *blockedInsightUsageStore) MarkRuntimeUsageReportsFlushed(_ context.Context, ids []uuid.UUID, _ time.Time) error {
	// Incorrect acknowledgement identity must fail rather than hide lost accounting.
	if len(ids) != 1 || ids[0] != s.report.ReportID {
		return errors.New("unexpected usage acknowledgement")
	}
	s.flushed = true
	return nil
}

type isolatedUsageClient struct {
	unavailable bool
	sent        []models.EngineUsageReport
}

// SendUsageReports captures the actual worker request and can reject it to
// prove that isolating discovery does not weaken retry durability.
func (c *isolatedUsageClient) SendUsageReports(_ context.Context, _, _ string, reports []models.EngineUsageReport, _ time.Time) error {
	c.sent = append(c.sent, reports...)
	// Simulated Registry failure must leave the outbox entry pending for a later pass.
	if c.unavailable {
		return errors.New("Registry unavailable")
	}
	return nil
}
