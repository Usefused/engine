package worker

import (
	"context"
	"time"
)

var workerFlushTimeout = 5 * time.Second

func boundedWorkerFlushContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	// Background accounting writes are best-effort durability work. They should
	// preserve audit/usage records, but never let a stuck database or Registry
	// connection pin the worker forever.
	return context.WithTimeout(parent, workerFlushTimeout)
}

func finalWorkerFlushContext() (context.Context, context.CancelFunc) {
	return boundedWorkerFlushContext(context.Background())
}
