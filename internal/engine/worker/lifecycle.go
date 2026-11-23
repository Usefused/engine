package worker

import (
	"context"
	"sync"
)

// workerLifecycle centralizes cancellable background worker shutdown so every
// worker drains its goroutine with identical idempotent stop semantics.
type workerLifecycle struct {
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// newStartedLifecycle derives a child context and initializes completion
// tracking for workers that begin running immediately on construction.
func newStartedLifecycle(ctx context.Context) (context.Context, *workerLifecycle) {
	workerCtx, cancel := context.WithCancel(ctx)
	return workerCtx, &workerLifecycle{
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// newDeferredLifecycle initializes a stopped worker that launches at most once
// when start is invoked.
func newDeferredLifecycle() *workerLifecycle {
	return &workerLifecycle{
		done: make(chan struct{}),
	}
}

// start launches fn exactly once in a goroutine bound to a derived context.
func (l *workerLifecycle) start(ctx context.Context, fn func(context.Context)) {
	// Skip starting when the lifecycle was not constructed via newDeferredLifecycle.
	if l == nil {
		return
	}
	l.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		l.cancel = cancel
		go fn(workerCtx)
	})
}

// stop cancels the worker context once and waits for completion or caller timeout.
func (l *workerLifecycle) stop(ctx context.Context) {
	// An uninitialized or unstarted worker has no active goroutine to cancel or join.
	if l == nil || l.cancel == nil {
		return
	}
	l.stopOnce.Do(l.cancel)
	// Bound shutdown wait to the caller context so engine teardown cannot hang.
	select {
	case <-l.done:
	case <-ctx.Done():
	}
}
