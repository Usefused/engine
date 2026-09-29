package executionappvm

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// TestWorkerConfinementExitCode ensures only explicit OS policy rejection can make integration coverage inapplicable.
func TestWorkerConfinementExitCode(t *testing.T) {
	for _, denial := range []error{syscall.EPERM, syscall.EACCES, syscall.ENOSYS} {
		// Wrapped syscall errors must preserve the startup denial classification.
		if got := workerConfinementExitCode(fmt.Errorf("confine: %w", denial)); got != WorkerIsolationDeniedExitCode {
			t.Fatalf("denial %v returned exit %d", denial, got)
		}
	}
	for _, failure := range []error{syscall.EINVAL, syscall.ENOENT, errors.New("unexpected startup failure")} {
		// Broken jail paths and unsupported implementations are regressions, not host-policy skips.
		if got := workerConfinementExitCode(failure); got != 1 {
			t.Fatalf("failure %v returned exit %d", failure, got)
		}
	}
}
