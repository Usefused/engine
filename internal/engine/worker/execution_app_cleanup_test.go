package worker

import (
	"context"
	"testing"
	"time"
)

type executionAppCleanupFixture struct {
	recoveryCalls   int
	evidenceCalls   int
	resultCalls     int
	evidenceDeleted int64
	resultDeleted   int64
}

// RecoverStaleExecutionResults counts bounded abandoned-work recovery batches.
func (fixture *executionAppCleanupFixture) RecoverStaleExecutionResults(_ context.Context, _ time.Time, limit int) (int64, error) {
	fixture.recoveryCalls++
	return 0, nil
}

// DeleteExpiredReplayEvidence counts bounded evidence batches for worker tests.
func (fixture *executionAppCleanupFixture) DeleteExpiredReplayEvidence(_ context.Context, _ time.Time, limit int) (int64, error) {
	fixture.evidenceCalls++
	return fixture.evidenceDeleted, nil
}

// DeleteExpiredExecutionResults counts bounded result batches for worker tests.
func (fixture *executionAppCleanupFixture) DeleteExpiredExecutionResults(_ context.Context, _ time.Time, limit int) (int64, error) {
	fixture.resultCalls++
	return fixture.resultDeleted, nil
}

// TestExecutionAppCleanupPassBoundsWork verifies one pass cannot drain an unbounded backlog.
func TestExecutionAppCleanupPassBoundsWork(t *testing.T) {
	fixture := &executionAppCleanupFixture{evidenceDeleted: executionAppCleanupBatch, resultDeleted: executionAppCleanupBatch}
	cleanupExecutionAppPass(context.Background(), fixture, time.Now())
	if fixture.recoveryCalls != 1 || fixture.evidenceCalls != executionAppCleanupMaxBatches || fixture.resultCalls != executionAppCleanupMaxBatches {
		t.Fatalf("cleanup exceeded or missed batch cap: evidence=%d results=%d", fixture.evidenceCalls, fixture.resultCalls)
	}
	fixture = &executionAppCleanupFixture{evidenceDeleted: 1, resultDeleted: 0}
	cleanupExecutionAppPass(context.Background(), fixture, time.Now())
	if fixture.recoveryCalls != 1 || fixture.evidenceCalls != 1 || fixture.resultCalls != 1 {
		t.Fatalf("small cleanup needed more than one batch: evidence=%d results=%d", fixture.evidenceCalls, fixture.resultCalls)
	}
}

// TestExecutionAppCleanupWorkerStops waits for startup cleanup and bounded shutdown.
func TestExecutionAppCleanupWorkerStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &executionAppCleanupFixture{}
	worker := StartExecutionAppCleanupWorker(ctx, fixture)
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	worker.Stop(stopCtx)
	if stopCtx.Err() != nil {
		t.Fatalf("cleanup worker did not stop: %v", stopCtx.Err())
	}
}
