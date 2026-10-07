package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNestedCapabilityAdmission avoids parent-child deadlocks without bypassing deployment concurrency.
func TestNestedCapabilityAdmission(t *testing.T) {
	ctx := context.Background()
	gate := newCapabilityAdmission(2, 16, time.Minute)
	parent, err := gate.acquireInvocation(ctx, ctx, "parent", 2)
	// A root keeps normal admission before it delegates.
	if err != nil {
		t.Fatal(err)
	}
	defer parent()
	nested := WithNestedCapabilityCall(ctx)
	child, err := gate.acquireInvocation(nested, ctx, "child", 2)
	// A free second slot permits the child while the parent retains its own slot.
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = gate.acquireInvocation(nested, ctx, "grandchild", 2)
	// A full gate must reject promptly instead of waiting for an ancestor to finish.
	if !errors.Is(err, ErrCapabilityAdmissionFull) || time.Since(start) > time.Second {
		t.Fatalf("nested admission: %v", err)
	}
	child()
	child()
	// Idempotent release restores capacity without undercounting the waiting parent.
	if gate.active != 1 {
		t.Fatalf("active=%d", gate.active)
	}
	cancelled, cancel := context.WithCancel(nested)
	cancel()
	// Cancellation wins even when a slot has just become free.
	if _, err := gate.acquireInvocation(cancelled, ctx, "child", 2); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// A family already at its limit cannot borrow spare deployment capacity.
	if _, err := gate.acquireInvocation(nested, ctx, "parent", 1); !errors.Is(err, ErrCapabilityAdmissionFull) {
		t.Fatal(err)
	}
}
