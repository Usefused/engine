package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// waitForExecutionWaiters observes the account queue without assuming goroutine
// scheduling order, so concurrency tests assert the actual admission state.
func waitForExecutionWaiters(t *testing.T, accountID uuid.UUID, wanted int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		activeExecutionMu.Lock()
		value, exists := activeExecutions.Load(accountID.String())
		count := 0
		// An idle account is deliberately removed from the tracker.
		if exists {
			count = len(value.(*executionCounter).waiters)
		}
		activeExecutionMu.Unlock()
		// Wait for the exact queue length to prove admission or cleanup.
		if count == wanted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("account queue did not reach %d waiters", wanted)
}

// TestUnifiedAppPhysicalQueueTransfersSlot proves two active calls can hand
// capacity to a third app call without oversubscribing the entitlement.
func TestUnifiedAppPhysicalQueueTransfersSlot(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(2)})
	accountID := uuid.New()
	identity := auth.RuntimeIdentity{AccountID: accountID, AppFamilyID: uuid.New(), AppID: uuid.New()}
	otherApp := auth.RuntimeIdentity{AccountID: accountID, AppFamilyID: uuid.New(), AppID: uuid.New()}
	ctx := WithUnifiedAppPhysicalQueue(context.Background())
	span := trace.SpanFromContext(ctx)
	first, err := trackAuthenticatedExecution(ctx, identity, span)
	if err != nil {
		t.Fatal(err)
	}
	second, err := trackAuthenticatedExecution(ctx, identity, span)
	if err != nil {
		first()
		t.Fatal(err)
	}
	result := make(chan error, 1)
	releaseThird := make(chan func(), 1)
	go func() {
		release, waitErr := trackAuthenticatedExecution(ctx, otherApp, span)
		// A granted waiter owns its permit until the test releases it.
		if waitErr == nil {
			releaseThird <- release
		}
		result <- waitErr
	}()
	waitForExecutionWaiters(t, accountID, 1)
	first()
	select {
	case err = <-result:
		// The freed permit should wake the waiting app call promptly.
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued app call did not receive released slot")
	}
	activeExecutionMu.Lock()
	value, _ := activeExecutions.Load(accountID.String())
	current := value.(*executionCounter).count
	activeExecutionMu.Unlock()
	// A transfer keeps the account at two active physical operations.
	if current != 2 {
		t.Fatalf("expected two active calls after handoff, got %d", current)
	}
	(<-releaseThird)()
	second()
}

// TestUnifiedAppPhysicalQueueCancellation removes an abandoned waiter so
// later calls can receive the released permit.
func TestUnifiedAppPhysicalQueueCancellation(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(1)})
	accountID := uuid.New()
	identity := auth.RuntimeIdentity{AccountID: accountID}
	span := trace.SpanFromContext(context.Background())
	hold, err := trackAuthenticatedExecution(context.Background(), identity, span)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(WithUnifiedAppPhysicalQueue(context.Background()))
	result := make(chan error, 1)
	go func() {
		_, waitErr := trackAuthenticatedExecution(ctx, identity, span)
		result <- waitErr
	}()
	waitForExecutionWaiters(t, accountID, 1)
	cancel()
	select {
	case err = <-result:
		// Cancellation must be returned without waiting for the provider call.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued app call did not cancel")
	}
	waitForExecutionWaiters(t, accountID, 0)
	hold()
	// A canceled waiter must not consume the next available slot.
	release, err := trackAuthenticatedExecution(WithUnifiedAppPhysicalQueue(context.Background()), identity, span)
	if err != nil {
		t.Fatalf("next app call could not acquire slot: %v", err)
	}
	release()
}

// TestUnifiedAppPhysicalQueueIsBounded ensures a stalled provider cannot
// accumulate an unbounded number of waiting application requests.
func TestUnifiedAppPhysicalQueueIsBounded(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(1)})
	accountID := uuid.New()
	identity := auth.RuntimeIdentity{AccountID: accountID}
	span := trace.SpanFromContext(context.Background())
	hold, err := trackAuthenticatedExecution(context.Background(), identity, span)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(WithUnifiedAppPhysicalQueue(context.Background()))
	var waiting sync.WaitGroup
	waiting.Add(maxPhysicalOperationWaiters)
	for range maxPhysicalOperationWaiters {
		go func() {
			defer waiting.Done()
			_, _ = trackAuthenticatedExecution(ctx, identity, span)
		}()
	}
	waitForExecutionWaiters(t, accountID, maxPhysicalOperationWaiters)
	// The next call must be rejected before allocating another waiter.
	_, err = trackAuthenticatedExecution(ctx, identity, span)
	if !errors.Is(err, ErrUnifiedAppPhysicalQueueFull) {
		t.Fatalf("expected bounded queue error, got %v", err)
	}
	cancel()
	waiting.Wait()
	hold()
}

// TestUnifiedAppPhysicalQueueDoesNotChangeDirectGate proves an unmarked
// direct call cannot create a phantom permit while an app waits for capacity.
func TestUnifiedAppPhysicalQueueDoesNotChangeDirectGate(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(1)})
	accountID := uuid.New()
	identity := auth.RuntimeIdentity{AccountID: accountID}
	span := trace.SpanFromContext(context.Background())
	hold, err := trackAuthenticatedExecution(context.Background(), identity, span)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	releases := make(chan func(), 1)
	go func() {
		release, waitErr := trackAuthenticatedExecution(WithUnifiedAppPhysicalQueue(context.Background()), identity, span)
		// A granted app call retains the sole permit until the assertion completes.
		if waitErr == nil {
			releases <- release
		}
		result <- waitErr
	}()
	waitForExecutionWaiters(t, accountID, 1)
	_, err = trackAuthenticatedExecution(context.Background(), identity, span)
	// Unmarked direct calls still fail immediately at their existing entitlement limit.
	if _, denied := err.(*entitlement.LimitExceeded); !denied {
		t.Fatalf("expected direct-call concurrency denial, got %v", err)
	}
	hold()
	select {
	case err = <-result:
		// Rejection of the SDK call cannot prevent the app waiter from progressing.
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("app waiter did not receive original provider slot")
	}
	activeExecutionMu.Lock()
	value, _ := activeExecutions.Load(accountID.String())
	current := value.(*executionCounter).count
	activeExecutionMu.Unlock()
	// The rejected SDK attempt must not inflate physical concurrency.
	if current != 1 {
		t.Fatalf("expected one active physical call, got %d", current)
	}
	(<-releases)()
}
