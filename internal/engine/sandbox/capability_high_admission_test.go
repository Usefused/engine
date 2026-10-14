package sandbox

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/shared/config"
)

// TestCapabilityAdmissionAboveLegacyOccupancy ensures higher worker capacity is not silently restricted by the old 32-request gate.
func TestCapabilityAdmissionAboveLegacyOccupancy(t *testing.T) {
	policy := config.DefaultUnifiedAppAdmissionConfig()
	policy.MaxConcurrency, policy.MaxWorkerConcurrency = 64, 40
	gate, err := capabilityAdmissionFromConfig(policy)
	// A valid high-capacity policy must reach the same scheduler used in production.
	if err != nil {
		t.Fatal(err)
	}
	var releases []func()
	for range 40 {
		release, err := gate.acquire(context.Background(), context.Background(), "high", 40)
		// All forty requests must acquire immediately without hitting the historical occupancy bound.
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	waiting := queuedAdmissionLimit(t, gate, "high", 1, 40)
	releases[0]()
	admissionLease(t, waiting)()
	for _, release := range releases {
		release()
	}
	// Repeated release remains idempotent and queued work must leave no capacity behind.
	if gate.active != 0 || gate.queued != 0 {
		t.Fatalf("leaked active=%d queued=%d", gate.active, gate.queued)
	}
}
