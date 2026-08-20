package cmd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// connectedAuthRefreshStartStore returns one startup claim and then reports a
// drained due set so the lifecycle test isolates worker start and stop.
type connectedAuthRefreshStartStore struct {
	mu     sync.Mutex
	served bool
}

// ClaimAuthConnectionsForRefresh implements the lifecycle helper's narrow
// discovery dependency.
func (s *connectedAuthRefreshStartStore) ClaimAuthConnectionsForRefresh(_ context.Context, _, _, _, _ time.Time, _ int) ([]store.AuthConnectionRefreshClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.served {
		return nil, nil
	}
	s.served = true
	return []store.AuthConnectionRefreshClaim{{
		Connection: store.AuthConnection{ID: uuid.New()}, LeaseToken: uuid.New(),
	}}, nil
}

// connectedAuthRefreshStartExecutor blocks until Engine shutdown reaches the
// provider executor and then records completion.
type connectedAuthRefreshStartExecutor struct {
	started  chan struct{}
	finished chan struct{}
	once     sync.Once
}

// RefreshClaimedConnection proves the worker forwards lifecycle cancellation
// through its executor boundary.
func (e *connectedAuthRefreshStartExecutor) RefreshClaimedConnection(ctx context.Context, _ store.AuthConnectionRefreshClaim) (sandbox.AuthRefreshResult, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	close(e.finished)
	return sandbox.AuthRefreshResult{}, ctx.Err()
}

// TestStartConnectedAuthRefreshWorkerLifecycle proves the Engine helper starts
// an immediate pass and engineWorkers.Stop joins it within the shutdown budget.
func TestStartConnectedAuthRefreshWorkerLifecycle(t *testing.T) {
	refreshStore := &connectedAuthRefreshStartStore{}
	executor := &connectedAuthRefreshStartExecutor{started: make(chan struct{}), finished: make(chan struct{})}
	refreshWorker, err := startConnectedAuthRefreshWorker(context.Background(), refreshStore, executor, 2)
	if err != nil {
		t.Fatalf("startConnectedAuthRefreshWorker: %v", err)
	}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("startup refresh did not reach executor")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engineWorkers{connectedAuthRefresh: refreshWorker}.Stop(stopCtx)
	select {
	case <-executor.finished:
	case <-time.After(time.Second):
		t.Fatal("Engine worker stop did not cancel refresh executor")
	}
}

// TestStartConnectedAuthRefreshWorkerRejectsInvalidConcurrency ensures the
// lifecycle helper cannot bypass constructor validation.
func TestStartConnectedAuthRefreshWorkerRejectsInvalidConcurrency(t *testing.T) {
	refreshStore := &connectedAuthRefreshStartStore{}
	executor := &connectedAuthRefreshStartExecutor{started: make(chan struct{}), finished: make(chan struct{})}
	if _, err := startConnectedAuthRefreshWorker(context.Background(), refreshStore, executor, 65); err == nil {
		t.Fatal("start helper accepted excessive refresh concurrency")
	}
}
