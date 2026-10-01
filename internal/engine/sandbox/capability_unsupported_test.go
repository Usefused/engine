//go:build !linux && !darwin

package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCapabilityUnsupportedPlatformRejectsAuthoredCode prevents portable builds from bypassing missing OS confinement.
func TestCapabilityUnsupportedPlatformRejectsAuthoredCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := InspectCapabilityBundle(ctx, []byte(`while (true) {}`))
	// Even an infinite authored declaration must fail at platform admission rather than reach an interpreter.
	if !errors.Is(err, ErrCapabilityWorkerUnavailable) || ctx.Err() != nil {
		t.Fatalf("unsupported inspection bypassed admission: %v", err)
	}
	manager := NewCapabilityWorkerManager()
	defer manager.Close()
	err = manager.Warm(ctx, "family", "v1", []byte(`while (true) {}`))
	// A failed platform launch must also release the family's reserved account capacity.
	if !errors.Is(err, ErrCapabilityWorkerUnavailable) || len(manager.entries) != 0 || ctx.Err() != nil {
		t.Fatalf("unsupported family admission leaked a worker: %v, entries=%d", err, len(manager.entries))
	}
}
