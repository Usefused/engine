package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/shared/config"
	"io"
	"sync"
	"testing"
	"time"
)

type admissionResult struct {
	release func()
	err     error
}

// queuedAdmission arranges a waiting request before tests release the occupied slot.
func queuedAdmission(t *testing.T, gate *capabilityAdmission, family string, count int) <-chan admissionResult {
	t.Helper()
	return queuedAdmissionLimit(t, gate, family, count, 4)
}

// queuedAdmissionLimit exposes a family's plan ceiling independently from the global budget.
func queuedAdmissionLimit(t *testing.T, gate *capabilityAdmission, family string, count, limit int) <-chan admissionResult {
	t.Helper()
	result := make(chan admissionResult, 1)
	// Each result carries its own lease so ordering assertions cannot release another caller's capacity.
	go func() {
		release, err := gate.acquire(context.Background(), context.Background(), family, limit)
		result <- admissionResult{release, err}
	}()
	deadline := time.After(time.Second)
	for {
		gate.mu.Lock()
		queued := gate.queued
		gate.mu.Unlock()
		// Observe actual admission, never assume goroutine launch implies enqueue order.
		if queued == count {
			return result
		}
		select {
		case <-deadline:
			t.Fatal("request did not enter queue")
			return nil
		case <-time.After(time.Millisecond):
		}
	}
}

// admissionLease fails promptly if a scheduled caller never receives its expected turn.
func admissionLease(t *testing.T, result <-chan admissionResult) func() {
	t.Helper()
	select {
	case value := <-result:
		// A grant must succeed before its release function can be used.
		if value.err != nil {
			t.Fatal(value.err)
		}
		return value.release
	case <-time.After(time.Second):
		t.Fatal("scheduled request did not receive capacity")
		return nil
	}
}

// TestCapabilityAdmissionFairnessBounds proves rotation, overflow, and idempotent release across families.
func TestCapabilityAdmissionFairnessBounds(t *testing.T) {
	gate := newCapabilityAdmission(1, 3, time.Second)
	first, err := gate.acquire(context.Background(), context.Background(), "a", 4)
	// The initial request must occupy the sole global slot.
	if err != nil {
		t.Fatal(err)
	}
	a2 := queuedAdmission(t, gate, "a", 1)
	a3 := queuedAdmission(t, gate, "a", 2)
	b1 := queuedAdmission(t, gate, "b", 3)
	_, err = gate.acquire(context.Background(), context.Background(), "c", 4)
	// The global waiting room rejects excess work without allocating a worker.
	if !errors.Is(err, ErrCapabilityAdmissionFull) {
		t.Fatalf("overflow: %v", err)
	}
	first()
	first()
	admissionLease(t, a2)()
	admissionLease(t, b1)()
	admissionLease(t, a3)()
	// Completed historical families must not leave growing scheduler state.
	if gate.active != 0 || gate.queued != 0 || len(gate.running) != 0 || len(gate.queues) != 0 {
		t.Fatalf("leaked scheduler state: %+v", gate)
	}
}

// TestCapabilityAdmissionSkipsSaturatedFamily proves idle global slots remain usable by other families.
func TestCapabilityAdmissionSkipsSaturatedFamily(t *testing.T) {
	gate := newCapabilityAdmission(2, 10, time.Second)
	a, err := gate.acquire(context.Background(), context.Background(), "a", 1)
	// Occupy each family's sole slot before queuing their next requests.
	if err != nil {
		t.Fatal(err)
	}
	b, err := gate.acquire(context.Background(), context.Background(), "b", 1)
	// Both running slots must exist before the scheduling-order assertion.
	if err != nil {
		t.Fatal(err)
	}
	a2 := queuedAdmissionLimit(t, gate, "a", 1, 1)
	b2 := queuedAdmissionLimit(t, gate, "b", 2, 1)
	b()
	admissionLease(t, b2)()
	// Family A remains saturated despite being first in the rotation.
	select {
	case <-a2:
		t.Fatal("family concurrency exceeded")
	default:
	}
	a()
	admissionLease(t, a2)()
}

// TestCapabilityAdmissionTimeoutCancellation ensures finite waits, shutdown, and cancellation reclaim queued state.
func TestCapabilityAdmissionTimeoutCancellation(t *testing.T) {
	gate := newCapabilityAdmission(1, 10, 10*time.Millisecond)
	release, _ := gate.acquire(context.Background(), context.Background(), "busy", 4)
	_, err := gate.acquire(context.Background(), context.Background(), "waiting", 4)
	// Queue expiry must not masquerade as a worker exception.
	if !errors.Is(err, ErrCapabilityAdmissionTimeout) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = gate.acquire(ctx, context.Background(), "cancelled", 4)
	// Already abandoned requests cannot consume admission space.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Engine shutdown must wake queued calls independently of their HTTP context.
	go func() { _, err := gate.acquire(context.Background(), lifetime, "shutdown", 4); done <- err }()
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown admitted work")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
	release()
	if gate.active != 0 || gate.queued != 0 {
		t.Fatal("cancelled queue leaked capacity")
	}
}

// TestCapabilityAdmissionCancellationWaitsForWorker prevents disconnected callers from oversubscribing CPU.
func TestCapabilityAdmissionCancellationWaitsForWorker(t *testing.T) {
	gate := newCapabilityAdmission(1, 10, time.Second)
	release, _ := gate.acquire(context.Background(), context.Background(), "family", 4)
	ctx, cancel := context.WithCancel(context.Background())
	invocation := &capabilityInvocation{ctx: ctx, result: make(chan capabilityInvocationResult, 1), releaseAdmission: release}
	worker := &persistentCapabilityWorker{slots: make(chan struct{}, 1), closed: make(chan struct{}), pending: map[uint64]*capabilityInvocation{1: invocation}, writer: &capabilityFrameWriter{encoder: json.NewEncoder(io.Discard)}}
	worker.slotCond = sync.NewCond(&worker.mu)
	worker.slots <- struct{}{}
	cancel()
	_, err := worker.await(ctx, 1, invocation)
	// The HTTP caller returns, but actual interpreter completion still owns the lease.
	if !errors.Is(err, context.Canceled) || gate.active != 1 {
		t.Fatalf("early release: %v, active=%d", err, gate.active)
	}
	worker.finish(1, capabilityInvocationResult{})
	if gate.active != 0 {
		t.Fatal("completion did not release global capacity")
	}
}

// TestCapabilityAdmissionConfiguration rejects unsafe deployment overrides before serving execution traffic.
func TestCapabilityAdmissionConfiguration(t *testing.T) {
	for _, value := range []string{"0", "-1", "", "garbage", "1025"} {
		t.Setenv("FUSED_UNIFIED_APP_MAX_CONCURRENCY", value)
		// No invalid value may disable the protective budget implicitly.
		if !errors.Is(ValidateCapabilityAdmissionConfig(), ErrCapabilityAdmissionConfig) {
			t.Fatalf("accepted %q", value)
		}
	}
	t.Setenv("FUSED_UNIFIED_APP_MAX_CONCURRENCY", "2")
	t.Setenv("FUSED_UNIFIED_APP_QUEUE_CAPACITY", "256")
	t.Setenv("FUSED_UNIFIED_APP_QUEUE_TIMEOUT_SECONDS", "5")
	if err := ValidateCapabilityAdmissionConfig(); err != nil {
		t.Fatal(err)
	}
}

// TestCapabilityAdmissionCancelGrantRace ensures cancellation racing a grant never leaks global capacity.
func TestCapabilityAdmissionCancelGrantRace(t *testing.T) {
	for range 200 {
		gate := newCapabilityAdmission(1, 4, time.Second)
		held, _ := gate.acquire(context.Background(), context.Background(), "first", 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		// Either cancellation or successful admission is legal, but both paths must return every permit.
		go func() {
			release, err := gate.acquire(ctx, context.Background(), "second", 1)
			if err == nil {
				release()
			}
			close(done)
		}()
		go cancel()
		held()
		<-done
		gate.mu.Lock()
		// Check under lock because an unsuccessful canceled waiter may still have raced dispatch.
		if gate.active != 0 || gate.queued != 0 {
			t.Fatalf("leaked active=%d queued=%d", gate.active, gate.queued)
		}
		gate.mu.Unlock()
	}
}

// TestCapabilityAdmissionFamilyBound ensures one family cannot consume the entire global waiting room.
func TestCapabilityAdmissionFamilyBound(t *testing.T) {
	gate := newCapabilityAdmission(1, 256, time.Second)
	first, _ := gate.acquire(context.Background(), context.Background(), "a", 4)
	var waiting []<-chan admissionResult
	for count := 1; count < capabilityWorkerQueueLimit; count++ {
		waiting = append(waiting, queuedAdmission(t, gate, "a", count))
	}
	_, err := gate.acquire(context.Background(), context.Background(), "a", 4)
	// The family cap includes its running request while preserving capacity for other families.
	if !errors.Is(err, ErrCapabilityAdmissionFull) {
		t.Fatalf("family overflow: %v", err)
	}
	other := queuedAdmission(t, gate, "b", capabilityWorkerQueueLimit)
	first()
	admissionLease(t, waiting[0])()
	admissionLease(t, other)()
	for _, result := range waiting[1:] {
		admissionLease(t, result)()
	}
}

// TestCapabilityAdmissionAcrossResidentFamilies verifies real workers share one deployment budget.
func TestCapabilityAdmissionAcrossResidentFamilies(t *testing.T) {
	requireResidentWorker(t)
	manager := NewCapabilityWorkerManager()
	manager.admission = newCapabilityAdmission(1, 256, 5*time.Second)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host := &managerBlockingHost{entered: make(chan struct{}, 2), release: make(chan struct{})}
	done := make(chan error, 2)
	// The first family occupies the only shared permit while waiting at its provider boundary.
	go func() { _, err := managerTestRun(ctx, manager, "a", "v1", "first", host); done <- err }()
	select {
	case <-host.entered:
	case <-ctx.Done():
		t.Fatal("first worker did not start")
	}
	// A different family must queue rather than starting another interpreter concurrently.
	go func() { _, err := managerTestRun(ctx, manager, "b", "v1", "second", host); done <- err }()
	deadline := time.After(time.Second)
	for {
		manager.admission.mu.Lock()
		queued := manager.admission.queued
		manager.admission.mu.Unlock()
		if queued == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second family did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-host.entered:
		t.Fatal("second family bypassed shared budget")
	default:
	}
	close(host.release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("queued work did not complete")
		}
	}
}

// TestCapabilityAdmissionResolvedYAMLIsNotOverriddenAgain proves the exact loader policy reaches the scheduler.
func TestCapabilityAdmissionResolvedYAMLIsNotOverriddenAgain(t *testing.T) {
	t.Setenv("FUSED_UNIFIED_APP_MAX_CONCURRENCY", "9")
	manager := NewCapabilityWorkerManagerWithConfig(config.UnifiedAppAdmissionConfig{MaxConcurrency: 2, QueueCapacity: 19, QueueTimeoutSeconds: 7})
	defer manager.Close()
	// An environment change after config resolution cannot replace the reviewed runtime settings.
	if manager.admissionErr != nil || manager.admission.limit != 2 || manager.admission.capacity != 19 || manager.admission.wait != 7*time.Second {
		t.Fatalf("resolved config not applied: %+v, %v", manager.admission, manager.admissionErr)
	}
}
