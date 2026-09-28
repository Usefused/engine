package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

const managerTestBundle = `globalThis.FusedExecutionManifest={schemaVersion:1,inputSchema:{},outputSchema:{},searchable:[],selectedOperations:[]};
globalThis.FusedExecutionApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async({input})=>JSON.parse(await __fusedHost.fetch(JSON.stringify({input:{value:input.name}})))};`

type managerBlockingHost struct {
	entered chan struct{}
	release chan struct{}
}

// Fetch blocks at the trusted boundary so tests can prove two VMs overlap in one process.
func (host *managerBlockingHost) Fetch(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Input struct {
			Value string `json:"value"`
		} `json:"input"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	host.entered <- struct{}{}
	select {
	case <-host.release:
		return json.Marshal(map[string]string{"id": request.Input.Value})
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DBGet is unavailable to this fixture because the authored bundle only calls fetch.
func (host *managerBlockingHost) DBGet(context.Context) (json.RawMessage, error) {
	return json.RawMessage("null"), nil
}

// DBSet is unavailable to this fixture because the authored bundle only calls fetch.
func (host *managerBlockingHost) DBSet(context.Context, json.RawMessage) error {
	return nil
}

// requireResidentWorker skips OS-dependent integration checks when isolation is unavailable.
func requireResidentWorker(t *testing.T) {
	t.Helper()
	if !IsCapabilityWorkerAvailable(context.Background()) {
		t.Skip("isolated execution worker is unavailable on this host")
	}
}

// managerTestRun invokes the exact version with fresh deterministic replay controls.
func managerTestRun(ctx context.Context, manager *CapabilityWorkerManager, familyID, appID, name string, host CapabilityScriptHost) (json.RawMessage, error) {
	control, err := NewCapabilityDeterminism()
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	return manager.Run(ctx, familyID, appID, []byte(managerTestBundle), input, host, control, true, nil)
}

// TestCapabilityWorkerManagerReusesProcessForConcurrentCalls verifies one family hosts overlapping requests.
func TestCapabilityWorkerManagerReusesProcessForConcurrentCalls(t *testing.T) {
	requireResidentWorker(t)
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := manager.Warm(ctx, "family", "v1", []byte(managerTestBundle)); err != nil {
		t.Fatal(err)
	}
	worker := manager.entries["family"].worker
	host := &managerBlockingHost{entered: make(chan struct{}, 2), release: make(chan struct{})}
	outputs := make(chan string, 2)
	var group sync.WaitGroup
	for _, name := range []string{"Ada", "Grace"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			value, err := managerTestRun(ctx, manager, "family", "v1", name, host)
			if err != nil {
				outputs <- err.Error()
				return
			}
			outputs <- string(value)
		}(name)
	}
	// Both provider calls must begin before either is released.
	for range 2 {
		select {
		case <-host.entered:
		case <-ctx.Done():
			t.Fatal("concurrent calls did not reach the same worker")
		}
	}
	close(host.release)
	group.Wait()
	assertManagerConcurrentOutputs(t, outputs)
	if manager.entries["family"].worker != worker {
		t.Fatal("concurrent calls created a second worker")
	}
}

// assertManagerConcurrentOutputs verifies both identities survive out-of-order completion.
func assertManagerConcurrentOutputs(t *testing.T, outputs <-chan string) {
	t.Helper()
	first, second := <-outputs, <-outputs
	// Both names must be present exactly once; the completion order is deliberately unconstrained.
	if !((first == `{"id":"Ada"}` && second == `{"id":"Grace"}`) || (first == `{"id":"Grace"}` && second == `{"id":"Ada"}`)) {
		t.Fatalf("unexpected outputs: %q, %q", first, second)
	}
}

// TestCapabilityWorkerManagerPromotionDrainsOldVersion prevents two versions sharing a family.
func TestCapabilityWorkerManagerPromotionDrainsOldVersion(t *testing.T) {
	requireResidentWorker(t)
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	host := &managerBlockingHost{entered: make(chan struct{}, 1), release: make(chan struct{})}
	runDone := make(chan error, 1)
	go func() {
		_, err := managerTestRun(ctx, manager, "family", "v1", "Ada", host)
		runDone <- err
	}()
	select {
	case <-host.entered:
	case <-ctx.Done():
		t.Fatal("first version did not begin")
	}
	old := manager.entries["family"].worker
	warmDone := make(chan error, 1)
	go func() { warmDone <- manager.Warm(ctx, "family", "v2", []byte(managerTestBundle+"\n// v2")) }()
	select {
	case err := <-warmDone:
		t.Fatalf("promotion interrupted the old request: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(host.release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-warmDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.closed:
	default:
		t.Fatal("old version remains resident after promotion")
	}
	if manager.entries["family"].worker == old {
		t.Fatal("promotion reused the old worker")
	}
}

// TestCapabilityWorkerManagerDowngradeRetiresIdleVersion keeps active callers alive on a plan change.
func TestCapabilityWorkerManagerDowngradeRetiresIdleVersion(t *testing.T) {
	requireResidentWorker(t)
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := manager.Warm(ctx, "family", "v1", []byte(managerTestBundle)); err != nil {
		t.Fatal(err)
	}
	entry := manager.entries["family"]
	if err := manager.ReconcileWarmTargets(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if entry.keepWarm {
		t.Fatal("downgrade retained always-on state")
	}
	manager.retireIdle("family", entry)
	if manager.entries["family"] != nil {
		t.Fatal("idle Dev worker did not retire")
	}
}

// TestCapabilityWorkerManagerIdleReconcileKeepsDeadline prevents refreshes from pinning a Dev worker.
func TestCapabilityWorkerManagerIdleReconcileKeepsDeadline(t *testing.T) {
	manager := NewCapabilityWorkerManager()
	defer manager.cancel()
	entry := &capabilityWorkerEntry{}
	manager.entries["family"] = entry
	manager.scheduleIdleLocked("family", entry)
	first := entry.idle
	defer first.Stop()
	manager.SetAlwaysOn(false)
	manager.SetAlwaysOn(false)
	// The first deadline must survive repeated ten-second entitlement refreshes.
	if entry.idle != first {
		t.Fatal("idle deadline was reset by entitlement reconciliation")
	}
}

// TestCapabilityWorkerManagerReplacesDeadChild starts a new child on the next request.
func TestCapabilityWorkerManagerReplacesDeadChild(t *testing.T) {
	requireResidentWorker(t)
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := manager.Warm(ctx, "family", "v1", []byte(managerTestBundle)); err != nil {
		t.Fatal(err)
	}
	old := manager.entries["family"].worker
	_ = old.command.Process.Kill()
	select {
	case <-old.closed:
	case <-ctx.Done():
		t.Fatal("worker death was not detected")
	}
	host := &managerBlockingHost{entered: make(chan struct{}, 1), release: make(chan struct{})}
	close(host.release)
	output, err := managerTestRun(ctx, manager, "family", "v1", "Ada", host)
	if err != nil || string(output) != `{"id":"Ada"}` {
		t.Fatalf("replacement output = %s, %v", output, err)
	}
	if manager.entries["family"].worker == old {
		t.Fatal("dead child was reused")
	}
}

// TestCapabilityWorkerManagerCancelIsolatesRequests keeps a canceled caller from stopping its sibling.
func TestCapabilityWorkerManagerCancelIsolatesRequests(t *testing.T) {
	requireResidentWorker(t)
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	if err := manager.Warm(ctx, "family", "v1", []byte(managerTestBundle)); err != nil {
		t.Fatal(err)
	}
	worker := manager.entries["family"].worker
	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstHost := &managerBlockingHost{entered: make(chan struct{}, 1), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		_, err := managerTestRun(firstCtx, manager, "family", "v1", "Ada", firstHost)
		firstDone <- err
	}()
	select {
	case <-firstHost.entered:
	case <-ctx.Done():
		t.Fatal("first request did not reach the host")
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request = %v", err)
	}
	secondHost := &managerBlockingHost{entered: make(chan struct{}, 1), release: make(chan struct{})}
	close(secondHost.release)
	output, err := managerTestRun(ctx, manager, "family", "v1", "Grace", secondHost)
	if err != nil || string(output) != `{"id":"Grace"}` {
		t.Fatalf("unrelated request = %s, %v", output, err)
	}
	if manager.entries["family"].worker != worker {
		t.Fatal("cancellation restarted the shared worker")
	}
}

// TestCapabilityWorkerQueueRejectsOverflow bounds waiting callers per family.
func TestCapabilityWorkerQueueRejectsOverflow(t *testing.T) {
	worker := &persistentCapabilityWorker{queue: make(chan struct{}, capabilityWorkerQueueLimit), slots: make(chan struct{}, capabilityWorkerCapacity), closed: make(chan struct{})}
	for range capabilityWorkerQueueLimit {
		worker.queue <- struct{}{}
	}
	_, err := worker.run(context.Background(), json.RawMessage(`{}`), &capabilityScriptTestHost{}, CapabilityDeterminism{}, capabilityWorkerCapacity)
	if !errors.Is(err, ErrCapabilityWorkerOverloaded) {
		t.Fatalf("queue overflow = %v", err)
	}
}

// TestCapabilityWorkerPlanConcurrencyNormalizesBounds keeps old bundles safe and explicit zero blocking.
func TestCapabilityWorkerPlanConcurrencyNormalizesBounds(t *testing.T) {
	values := []struct {
		name  string
		limit *int
		want  int
	}{
		{name: "older Registry", limit: nil, want: 4},
		{name: "Dev", limit: intPointer(2), want: 2},
		{name: "disabled", limit: intPointer(0), want: 0},
		{name: "legacy unlimited", limit: intPointer(-1), want: 4},
		{name: "oversized", limit: intPointer(5), want: 4},
	}
	for _, value := range values {
		// The tested process ceiling remains authoritative even if Registry sends a larger value.
		if got := effectiveCapabilityWorkerConcurrency(value.limit); got != value.want {
			t.Errorf("%s concurrency = %d, want %d", value.name, got, value.want)
		}
	}
}

// intPointer makes table cases explicit without mutating a shared plan limit.
func intPointer(value int) *int { return &value }

// TestCapabilityWorkerAdmitUsesPlanSlots proves a waiting Dev call can enter when one of two slots frees.
func TestCapabilityWorkerAdmitUsesPlanSlots(t *testing.T) {
	worker := &persistentCapabilityWorker{queue: make(chan struct{}, capabilityWorkerQueueLimit), slots: make(chan struct{}, capabilityWorkerCapacity), closed: make(chan struct{})}
	worker.slotCond = sync.NewCond(&worker.mu)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for range 2 {
		// The first two Dev invocations may overlap in the same process.
		if err := worker.admit(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
	third := make(chan error, 1)
	go func() { third <- worker.admit(ctx, 2) }()
	select {
	case err := <-third:
		t.Fatalf("third Dev invocation bypassed the two-slot limit: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	worker.mu.Lock()
	worker.releaseSlotLocked()
	worker.mu.Unlock()
	// Releasing one slot must wake the queued invocation without starting another process.
	if err := <-third; err != nil {
		t.Fatal(err)
	}
	// A later Scale-up limit may admit more work on this same resident process.
	if err := worker.admit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if got := len(worker.slots); got != 3 {
		t.Fatalf("active interpreter slots = %d, want 3", got)
	}
}

// TestCapabilityWorkerZeroPlanDoesNotStartProcess prevents an explicitly disabled plan from running code.
func TestCapabilityWorkerZeroPlanDoesNotStartProcess(t *testing.T) {
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	control, err := NewCapabilityDeterminism()
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	_, err = manager.Run(context.Background(), "family", "v1", []byte(managerTestBundle), json.RawMessage(`{}`), &capabilityScriptTestHost{}, control, false, &zero)
	// A zero limit is a plan denial, not a reason to allocate a resident worker.
	if !errors.Is(err, ErrCapabilityWorkerConcurrencyDisabled) || len(manager.entries) != 0 {
		t.Fatalf("zero concurrency = %v, workers=%d", err, len(manager.entries))
	}
}
