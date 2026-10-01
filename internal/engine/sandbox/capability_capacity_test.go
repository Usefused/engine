package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Usefused/engine/internal/shared/models"
	"testing"
)

// TestCapabilityResidentCapacityRejectsNewFamily proves admission happens before any additional worker process is allocated.
func TestCapabilityResidentCapacityRejectsNewFamily(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxUnifiedAppFamilies: models.IntPtr(2)})
	manager := NewCapabilityWorkerManager()
	defer manager.cancel()
	// Synthetic occupied entries let the test exceed capacity without starting a worker process.
	for index := 0; index < 2; index++ {
		manager.entries[fmt.Sprint(index)] = &capabilityWorkerEntry{}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	_, err := manager.loadEntryLocked(context.Background(), "extra", "v1", []byte("invalid"), sha256.Sum256(nil), true)
	// Even invalid source must be rejected for capacity before process startup or declaration inspection.
	if !errors.Is(err, ErrCapabilityWorkerOverloaded) {
		t.Fatalf("capacity was not enforced: %v", err)
	}
}

// TestCapabilityTransientCapacityRejectsInspection ensures direct bundle attachments share a global inspection budget.
func TestCapabilityTransientCapacityRejectsInspection(t *testing.T) {
	// Tests are sequential because this is deliberately the production process-wide limiter.
	for index := 0; index < cap(capabilityProcessSlots); index++ {
		capabilityProcessSlots <- struct{}{}
		defer func() { <-capabilityProcessSlots }()
	}
	_, err := InspectCapabilityBundle(context.Background(), []byte("invalid"))
	// Overload must not fall back to an unbounded interpreter, even for small bundles.
	if !errors.Is(err, ErrCapabilityWorkerOverloaded) {
		t.Fatalf("inspection bypassed capacity: %v", err)
	}
}
