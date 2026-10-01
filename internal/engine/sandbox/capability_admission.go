package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var ErrCapabilityAdmissionFull = errors.New("unified app execution queue is full")
var ErrCapabilityAdmissionTimeout = errors.New("unified app execution queue wait expired")
var ErrCapabilityAdmissionConfig = errors.New("invalid unified app admission configuration")

// capabilityAdmission bounds admitted work and rotates waiting families without preempting active code.
type capabilityAdmission struct {
	mu                              sync.Mutex
	limit, capacity, active, queued int
	wait                            time.Duration
	running                         map[string]int
	families                        []string
	queues                          map[string][]*capabilityWaiter
}

type capabilityWaiter struct {
	family  string
	limit   int
	ready   chan struct{}
	granted bool
}

// configuredCapabilityAdmission leaves CPU headroom by default and rejects malformed deployment overrides.
func configuredCapabilityAdmission() (*capabilityAdmission, error) {
	policy, err := config.ResolveUnifiedAppAdmission(config.DefaultUnifiedAppAdmissionConfig())
	// Standalone managers retain environment support; production receives the already-resolved YAML policy.
	if err != nil {
		return nil, ErrCapabilityAdmissionConfig
	}
	return capabilityAdmissionFromConfig(policy)
}

// capabilityAdmissionFromConfig consumes resolved configuration without applying environment overrides twice.
func capabilityAdmissionFromConfig(policy config.UnifiedAppAdmissionConfig) (*capabilityAdmission, error) {
	// Embedded callers must obey the same bounds as normal Engine configuration loading.
	if err := policy.Validate(); err != nil {
		return nil, ErrCapabilityAdmissionConfig
	}
	return newCapabilityAdmission(policy.MaxConcurrency, policy.QueueCapacity, time.Duration(policy.QueueTimeoutSeconds)*time.Second), nil
}

// newCapabilityAdmission also permits deterministic small budgets in scheduler tests.
func newCapabilityAdmission(limit, capacity int, wait time.Duration) *capabilityAdmission {
	return &capabilityAdmission{limit: limit, capacity: capacity, wait: wait, running: make(map[string]int), queues: make(map[string][]*capabilityWaiter)}
}

// acquire returns an idempotent lease held until the child actually completes or is reaped.
func (gate *capabilityAdmission) acquire(ctx, lifetime context.Context, family string, limit int) (release func(), err error) {
	start := time.Now()
	defer func() { recordCapabilityAdmission(ctx, time.Since(start), err) }()
	gate.mu.Lock()
	// A canceled request must not consume an otherwise immediately available slot.
	if ctx.Err() != nil || lifetime.Err() != nil {
		gate.mu.Unlock()
		return nil, context.Canceled
	}
	// Per-family occupancy includes active and queued work, preserving the existing family bound.
	if gate.queued >= gate.capacity || gate.running[family]+len(gate.queues[family]) >= capabilityWorkerQueueLimit {
		gate.mu.Unlock()
		return nil, ErrCapabilityAdmissionFull
	}
	waiter := &capabilityWaiter{family: family, limit: limit, ready: make(chan struct{})}
	// A family joins the rotation once regardless of its request count.
	if len(gate.queues[family]) == 0 {
		gate.families = append(gate.families, family)
	}
	gate.queues[family] = append(gate.queues[family], waiter)
	gate.queued++
	gate.dispatchLocked()
	gate.mu.Unlock()
	timer := time.NewTimer(gate.wait)
	defer timer.Stop()
	// Waiting is bounded by the caller, the Engine lifetime, and the deployment queue deadline.
	select {
	case <-waiter.ready:
	case <-ctx.Done():
		err = ctx.Err()
	case <-lifetime.Done():
		err = ErrCapabilityWorkerUnavailable
	case <-timer.C:
		err = ErrCapabilityAdmissionTimeout
	}
	gate.mu.Lock()
	// A racing cancellation must return a granted slot rather than leaking or starting abandoned work.
	if err == nil {
		err = ctx.Err()
	}
	// Engine shutdown wins even when a permit became ready at the same instant.
	if err == nil && lifetime.Err() != nil {
		err = ErrCapabilityWorkerUnavailable
	}
	// Retraction handles both queued requests and grants that raced the error.
	if err != nil {
		gate.cancelLocked(waiter)
		gate.dispatchLocked()
		gate.mu.Unlock()
		return nil, err
	}
	gate.mu.Unlock()
	return sync.OnceFunc(func() {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		gate.releaseLocked(family)
		gate.dispatchLocked()
	}), nil
}

// dispatchLocked skips saturated families and gives each eligible family one turn before cycling back.
func (gate *capabilityAdmission) dispatchLocked() {
	for gate.active < gate.limit && len(gate.families) > 0 {
		dispatched := false
		for index, family := range gate.families {
			waiter := gate.queues[family][0]
			// A busy family's plan limit must not block an unrelated ready family.
			if gate.running[family] >= waiter.limit {
				continue
			}
			gate.families = append(gate.families[:index], gate.families[index+1:]...)
			gate.queues[family] = gate.queues[family][1:]
			// Move remaining siblings to the end instead of letting one family drain its whole queue.
			if len(gate.queues[family]) > 0 {
				gate.families = append(gate.families, family)
			} else {
				delete(gate.queues, family)
			}
			gate.queued--
			gate.active++
			gate.running[family]++
			waiter.granted = true
			close(waiter.ready)
			dispatched = true
			break
		}
		// All remaining families are at their individual limits; completion will wake this scheduler.
		if !dispatched {
			return
		}
	}
}

// releaseLocked removes idle family bookkeeping so historical traffic cannot grow scheduler memory.
func (gate *capabilityAdmission) releaseLocked(family string) {
	gate.active--
	gate.running[family]--
	// Only in-flight families need a running-count entry.
	if gate.running[family] == 0 {
		delete(gate.running, family)
	}
}

// cancelLocked retracts either a queued request or its racing grant while holding the scheduler lock.
func (gate *capabilityAdmission) cancelLocked(waiter *capabilityWaiter) {
	// Completion owns granted capacity even if the ready channel raced a cancellation.
	if waiter.granted {
		gate.releaseLocked(waiter.family)
		return
	}
	queue := gate.queues[waiter.family]
	for index, candidate := range queue {
		// Pointer identity removes only this caller, never its siblings.
		if candidate != waiter {
			continue
		}
		queue = append(queue[:index], queue[index+1:]...)
		gate.queued--
		break
	}
	gate.queues[waiter.family] = queue
	// Empty queues must leave the rotation before another dispatch inspects their head.
	if len(queue) == 0 {
		delete(gate.queues, waiter.family)
		for index, family := range gate.families {
			// Each family occurs once in the round-robin ring.
			if family == waiter.family {
				gate.families = append(gate.families[:index], gate.families[index+1:]...)
				break
			}
		}
	}
}

// recordCapabilityAdmission uses the Engine-owned provider and bounded outcomes without request data.
func recordCapabilityAdmission(ctx context.Context, elapsed time.Duration, err error) {
	outcome := "admitted"
	// Only stable infrastructure classifications are exported; errors may otherwise contain private data.
	switch {
	case errors.Is(err, ErrCapabilityAdmissionFull):
		outcome = "full"
	case errors.Is(err, ErrCapabilityAdmissionTimeout):
		outcome = "timeout"
	case err != nil:
		outcome = "cancelled"
	}
	attrs := []attribute.KeyValue{attribute.String("outcome", outcome)}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("admission.outcome", outcome), attribute.Float64("admission.wait_ms", float64(elapsed)/float64(time.Millisecond)))
	histogram, _ := otel.Meter("engine").Float64Histogram("engine.unified_app.admission.wait", metric.WithUnit("s"))
	counter, _ := otel.Meter("engine").Int64Counter("engine.unified_app.admission.requests")
	histogram.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attrs...))
	counter.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// ValidateCapabilityAdmissionConfig prevents an invalid queue policy from reaching a serving Engine.
func ValidateCapabilityAdmissionConfig() error {
	_, err := configuredCapabilityAdmission()
	return err
}
