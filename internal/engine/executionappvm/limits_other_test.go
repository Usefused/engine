//go:build !linux && !darwin

package executionappvm

import "testing"

// TestUnsupportedWorkerFailsClosed prevents portable Engine builds from enabling workers without OS isolation.
func TestUnsupportedWorkerFailsClosed(t *testing.T) {
	// Unsupported platforms must reject both protection layers, never silently skip resource caps.
	if confineWorker() == nil || limitCapabilityWorker() == nil {
		t.Fatal("unsupported worker accepted missing OS protections")
	}
	// Worker entry must reject the platform before waiting for or evaluating an authored bundle.
	if RunWorker() != 1 {
		t.Fatal("unsupported worker did not fail closed")
	}
}
