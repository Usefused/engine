package sandbox

import (
	"context"
	"sync"
)

type nestedCapabilityCallKey struct{}

// WithNestedCapabilityCall marks Engine-owned delegation whose parent already holds an interpreter slot.
func WithNestedCapabilityCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, nestedCapabilityCallKey{}, true)
}

// acquireInvocation never queues a child behind slots that may all belong to its waiting ancestors.
func (gate *capabilityAdmission) acquireInvocation(ctx, lifetime context.Context, family string, limit int) (func(), error) {
	// Root requests keep normal fair queueing; only synchronous delegation needs immediate admission.
	if nested, _ := ctx.Value(nestedCapabilityCallKey{}).(bool); !nested {
		return gate.acquire(ctx, lifetime, family, limit)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	// Cancellation must win before any nested invocation consumes capacity.
	if ctx.Err() != nil || lifetime.Err() != nil {
		return nil, context.Canceled
	}
	// Preserve both deployment and family limits without jumping ahead of queued root work.
	if gate.active >= gate.limit || gate.running[family] >= limit || gate.queued > 0 {
		return nil, ErrCapabilityAdmissionFull
	}
	gate.active++
	gate.running[family]++
	// Completion or worker reaping releases the same lease exactly once.
	return sync.OnceFunc(func() {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		gate.releaseLocked(family)
		gate.dispatchLocked()
	}), nil
}
