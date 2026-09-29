package worker

import (
	"context"
	"testing"
	"time"
)

type unifiedAppCleanupFixture struct {
	recoveryCalls   int
	evidenceCalls   int
	resultCalls     int
	evidenceDeleted int64
	resultDeleted   int64
}

// RecoverStaleExecutionResults counts bounded abandoned-work recovery batches.
func (fixture *unifiedAppCleanupFixture) RecoverStaleExecutionResults(_ context.Context, _ time.Time, limit int) (int64, error) {
	fixture.recoveryCalls++
	return 0, nil
}

// DeleteExpiredReplayEvidence counts bounded evidence batches for worker tests.
func (fixture *unifiedAppCleanupFixture) DeleteExpiredReplayEvidence(_ context.Context, _ time.Time, limit int) (int64, error) {
	fixture.evidenceCalls++
	return fixture.evidenceDeleted, nil
}

// DeleteExpiredExecutionResults counts bounded result batches for worker tests.
func (fixture *unifiedAppCleanupFixture) DeleteExpiredExecutionResults(_ context.Context, _ time.Time, limit int) (int64, error) {
	fixture.resultCalls++
	return fixture.resultDeleted, nil
}

// TestUnifiedAppCleanupPassBoundsWork verifies one pass cannot drain an unbounded backlog.
func TestUnifiedAppCleanupPassBoundsWork(t *testing.T) {
	fixture := &unifiedAppCleanupFixture{evidenceDeleted: unifiedAppCleanupBatch, resultDeleted: unifiedAppCleanupBatch}
	cleanupUnifiedAppPass(context.Background(), fixture, time.Now())
	if fixture.recoveryCalls != 1 || fixture.evidenceCalls != unifiedAppCleanupMaxBatches || fixture.resultCalls != unifiedAppCleanupMaxBatches {
		t.Fatalf("cleanup exceeded or missed batch cap: evidence=%d results=%d", fixture.evidenceCalls, fixture.resultCalls)
	}
	fixture = &unifiedAppCleanupFixture{evidenceDeleted: 1, resultDeleted: 0}
	cleanupUnifiedAppPass(context.Background(), fixture, time.Now())
	if fixture.recoveryCalls != 1 || fixture.evidenceCalls != 1 || fixture.resultCalls != 1 {
		t.Fatalf("small cleanup needed more than one batch: evidence=%d results=%d", fixture.evidenceCalls, fixture.resultCalls)
	}
}

// TestUnifiedAppCleanupWorkerStops waits for startup cleanup and bounded shutdown.
func TestUnifiedAppCleanupWorkerStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &unifiedAppCleanupFixture{}
	worker := StartUnifiedAppCleanupWorker(ctx, fixture)
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	worker.Stop(stopCtx)
	if stopCtx.Err() != nil {
		t.Fatalf("cleanup worker did not stop: %v", stopCtx.Err())
	}
}
