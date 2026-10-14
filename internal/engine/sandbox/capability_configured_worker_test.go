package sandbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/config"
)

// TestCapabilityConfiguredWorkerRunsAboveFour verifies the manager-to-child capacity handshake in a real confined process.
func TestCapabilityConfiguredWorkerRunsAboveFour(t *testing.T) {
	requireResidentWorker(t)
	policy := config.DefaultUnifiedAppAdmissionConfig()
	policy.MaxConcurrency, policy.MaxWorkerConcurrency = 16, 8
	manager := NewCapabilityWorkerManagerWithConfig(policy)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	host := &managerBlockingHost{entered: make(chan struct{}, 8), release: make(chan struct{})}
	done := make(chan error, 8)
	for range 8 {
		// Each invocation must retain its own controls and host correlation while sharing one worker process.
		go func() {
			control, err := NewCapabilityDeterminism()
			// Failed control initialization must not silently reduce the number of attempted invocations.
			if err == nil {
				plan := 8
				_, err = manager.Run(ctx, "configured-family", "v1", []byte(managerTestBundle), json.RawMessage(`{"name":"test"}`), host, control, true, &plan)
			}
			done <- err
		}()
	}
	awaitConfiguredWorkerOverlap(t, ctx, host, done)
	manager.mu.Lock()
	worker := manager.entries["configured-family"].worker
	manager.mu.Unlock()
	// Parent occupancy and child acknowledgments must agree on the configured capacity.
	if worker.active() != 8 || cap(worker.slots) != 8 {
		t.Fatalf("active=%d capacity=%d", worker.active(), cap(worker.slots))
	}
	close(host.release)
	for range 8 {
		// Every admitted request must finish successfully after releasing the shared blocking fixture.
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// awaitConfiguredWorkerOverlap requires all requests to enter before the shared host allows any completion.
func awaitConfiguredWorkerOverlap(t *testing.T, ctx context.Context, host *managerBlockingHost, done <-chan error) {
	t.Helper()
	for range 8 {
		// Every request must enter before any provider result is released, proving genuine overlap.
		select {
		case <-host.entered:
		case err := <-done:
			t.Fatalf("invocation finished before overlap: %v", err)
		case <-ctx.Done():
			t.Fatal("configured worker never admitted eight simultaneous invocations")
		}
	}
}
