package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// concurrencyFixture helps set up the entitlement singleton and mock cache for
// concurrency-limit tests. It returns a vendor URL, a cache, endpoint name, and
// a cleanup function.
func concurrencyFixture(t *testing.T, vendorHandler http.HandlerFunc) (cache *richMockCache, endpoint string, vendorURL string) {
	t.Helper()
	vendor := httptest.NewServer(vendorHandler)
	t.Cleanup(func() { vendor.Close() })
	cache, endpoint = makePassthroughCache(t, vendor.URL)
	return cache, endpoint, vendor.URL
}

// withEntitlement installs the given RuntimeEntitlement into the global
// LiveEntitlement singleton for the duration of the test.
func withEntitlement(t *testing.T, rt models.RuntimeEntitlement) {
	t.Helper()
	old := entitlement.LiveEntitlement.Load()
	entitlement.LiveEntitlement.Store(rt)
	t.Cleanup(func() { entitlement.LiveEntitlement.Store(old) })
}

// accountValidator returns a token validator that always resolves to the same
// account/app so concurrent calls accumulate under one AccountID.
type accountValidator struct {
	accountID uuid.UUID
	appID     uuid.UUID
}

func (a *accountValidator) Validate(_ context.Context, appID uuid.UUID, _ string) (auth.RuntimeIdentity, error) {
	return auth.RuntimeIdentity{
		AccountID:   a.accountID,
		AppFamilyID: uuid.New(),
		AppID:       appID,
		AppVersion:  "1.0.0",
		Kind:        "sdk",
		Status:      "active",
	}, nil
}

// --- Unit tests for trackExecutionStart ---

// TestTrackExecutionStart_IncrementAndDecrement verifies the counter
// increments on start and decrements on the returned cleanup function.
func TestTrackExecutionStart_IncrementAndDecrement(t *testing.T) {
	acc := uuid.New()

	// Ensure counter starts at zero.
	activeExecutions.Delete(acc)

	current, decrement := trackExecutionStart(acc)
	if current != 1 {
		t.Fatalf("expected current=1 after first track, got %d", current)
	}

	current2, _ := trackExecutionStart(acc)
	if current2 != 2 {
		t.Fatalf("expected current=2 after second track, got %d", current2)
	}

	decrement()
	// After one decrement, a third call should report 2 again (net +1 from original).
	current3, decrement3 := trackExecutionStart(acc)
	if current3 != 2 {
		t.Fatalf("expected current=2 after decrement then track, got %d", current3)
	}

	// Clean up remaining.
	decrement3()
	// decrement from current2's call still pending? No, it was the first decrement.
	// We have 2 starts, 1 decrement, 1 more start -> 2 active.
	// Decrement the last one.
	// To simplify, clear all.
	activeExecutions.Delete(acc)
}

// TestTrackExecutionStart_IsolatedPerAccount confirms counters are keyed by
// AccountID and do not leak across accounts.
func TestTrackExecutionStart_IsolatedPerAccount(t *testing.T) {
	acc1 := uuid.New()
	acc2 := uuid.New()

	activeExecutions.Delete(acc1)
	activeExecutions.Delete(acc2)

	c1, d1 := trackExecutionStart(acc1)
	if c1 != 1 {
		t.Fatalf("expected acc1 current=1, got %d", c1)
	}

	c2, _ := trackExecutionStart(acc2)
	if c2 != 1 {
		t.Fatalf("expected acc2 current=1, got %d", c2)
	}

	c1b, _ := trackExecutionStart(acc1)
	if c1b != 2 {
		t.Fatalf("expected acc1 current=2, got %d", c1b)
	}

	d1()
	// After decrementing acc1 once, acc1 should be back to 1.
	c1c, d1c := trackExecutionStart(acc1)
	if c1c != 2 { // previous 2, minus 1 decrement = 1, plus this new one = 2
		t.Fatalf("expected acc1 current=2 after partial decrement, got %d", c1c)
	}

	d1c()
	activeExecutions.Delete(acc1)
	activeExecutions.Delete(acc2)
}

// --- Integration tests for engineExecuteCore + concurrency limit ---

// TestEngineExecuteCore_Concurrency_AllowsWhenUnderLimit ensures execution
// succeeds when active count is below a positive limit.
func TestEngineExecuteCore_Concurrency_AllowsWhenUnderLimit(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		MaxSandboxConcurrency: models.IntPtr(5),
	})

	// Reset counter for this account.
	activeExecutions.Delete(accID)

	v := &accountValidator{accountID: accID}
	buf := engine.NewBufferStream()
	if err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("expected no error when under limit, got %v", err)
	}
	if vendorCalls != 1 {
		t.Fatalf("expected vendor call, got %d", vendorCalls)
	}
}

// TestEngineExecuteCore_Concurrency_BlocksWhenAtLimit verifies that the
// *LimitExceeded error is returned when the active count already equals the
// limit.
func TestEngineExecuteCore_Concurrency_BlocksWhenAtLimit(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		MaxSandboxConcurrency: models.IntPtr(2),
	})

	// Pre-seed two active executions for this account.
	activeExecutions.Delete(accID)
	_, d1 := trackExecutionStart(accID)
	_, d2 := trackExecutionStart(accID)
	defer d1()
	defer d2()

	v := &accountValidator{accountID: accID}
	buf := engine.NewBufferStream()
	err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf)
	if err == nil {
		t.Fatal("expected error when at concurrency limit, got nil")
	}
	if _, ok := err.(*entitlement.LimitExceeded); !ok {
		t.Fatalf("expected *entitlement.LimitExceeded, got %T: %v", err, err)
	}
}

// TestEngineExecuteCore_Concurrency_MissingLimitIsUnlimited confirms that a
// nil MaxSandboxConcurrency (missing / not set) is treated as unlimited and
// does not block execution.
func TestEngineExecuteCore_Concurrency_MissingLimitIsUnlimited(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		// MaxSandboxConcurrency left nil => unlimited
	})

	activeExecutions.Delete(accID)
	// Seed many active executions.
	var decs []func()
	for i := 0; i < 10; i++ {
		_, d := trackExecutionStart(accID)
		decs = append(decs, d)
	}
	defer func() {
		for _, d := range decs {
			d()
		}
	}()

	v := &accountValidator{accountID: accID}
	buf := engine.NewBufferStream()
	if err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("expected no error when limit is nil (unlimited), got %v", err)
	}
	if vendorCalls != 1 {
		t.Fatalf("expected vendor call, got %d", vendorCalls)
	}
}

// TestEngineExecuteCore_Concurrency_NegativeOneIsUnlimited confirms that a
// MaxSandboxConcurrency of -1 is treated as unlimited.
func TestEngineExecuteCore_Concurrency_NegativeOneIsUnlimited(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		MaxSandboxConcurrency: models.IntPtr(-1),
	})

	activeExecutions.Delete(accID)
	// Seed many active executions.
	var decs []func()
	for i := 0; i < 10; i++ {
		_, d := trackExecutionStart(accID)
		decs = append(decs, d)
	}
	defer func() {
		for _, d := range decs {
			d()
		}
	}()

	v := &accountValidator{accountID: accID}
	buf := engine.NewBufferStream()
	if err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("expected no error when limit is -1 (unlimited), got %v", err)
	}
	if vendorCalls != 1 {
		t.Fatalf("expected vendor call, got %d", vendorCalls)
	}
}

// TestEngineExecuteCore_Concurrency_DecrementsOnEarlyReturn ensures that when
// the limit is exceeded and engineExecuteCore returns early, the counter is
// still decremented so later calls under the limit can succeed.
func TestEngineExecuteCore_Concurrency_DecrementsOnEarlyReturn(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		MaxSandboxConcurrency: models.IntPtr(1),
	})

	activeExecutions.Delete(accID)
	v := &accountValidator{accountID: accID}

	// Pre-seed one active execution so the next call hits the limit immediately.
	_, d0 := trackExecutionStart(accID)

	buf2 := engine.NewBufferStream()
	err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf2)
	if err == nil {
		t.Fatal("expected error when limit exceeded, got nil")
	}

	// Remove the pre-seeded execution so the counter drops to 0.
	// The blocked call also decremented before returning, so after d0() we are at 0.
	d0()

	// A new call should now succeed because the account has 0 active executions.
	buf3 := engine.NewBufferStream()
	if err3 := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf3); err3 != nil {
		t.Fatalf("expected success after early-return decrement, got %v", err3)
	}
}

// TestEngineExecuteCore_Concurrency_DecrementsAfterCompletion verifies the
// counter is decremented after a normal (non-limited) execution completes.
func TestEngineExecuteCore_Concurrency_DecrementsAfterCompletion(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		MaxSandboxConcurrency: models.IntPtr(2),
	})

	activeExecutions.Delete(accID)

	v := &accountValidator{accountID: accID}
	buf1 := engine.NewBufferStream()
	if err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf1); err != nil {
		t.Fatalf("first call error: %v", err)
	}

	// After first call completes, counter should be 0.
	// Pre-seed 1 active so the next call sees 1 < 2 and succeeds.
	_, dHold := trackExecutionStart(accID)
	defer dHold()

	buf2 := engine.NewBufferStream()
	if err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf2); err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if vendorCalls != 2 {
		t.Fatalf("expected 2 vendor calls, got %d", vendorCalls)
	}
}

// TestEngineExecuteCore_Concurrency_ZeroBlocksAll ensures MaxSandboxConcurrency=0
// blocks every execution for that account.
func TestEngineExecuteCore_Concurrency_ZeroBlocksAll(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	accID := uuid.New()
	withEntitlement(t, models.RuntimeEntitlement{
		MaxSandboxConcurrency: models.IntPtr(0),
	})

	activeExecutions.Delete(accID)

	v := &accountValidator{accountID: accID}
	buf := engine.NewBufferStream()
	err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), v, accID.String(), "tok", endpoint, map[string]any{}, nil, "", buf)
	if err == nil {
		t.Fatal("expected error when limit=0, got nil")
	}
	if _, ok := err.(*entitlement.LimitExceeded); !ok {
		t.Fatalf("expected *entitlement.LimitExceeded, got %T: %v", err, err)
	}
}
