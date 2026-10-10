//go:build linux || darwin

package executionappvm

import (
	"fmt"
	"syscall"
)

// limitCapabilityWorker bounds process memory, CPU time, and file descriptors independently of Goja limits.
func limitCapabilityWorker() error {
	// Install platform-specific memory bounds before any authored code can be read.
	if err := limitCapabilityMemory(); err != nil {
		return err
	}
	limits := []struct {
		resource int
		limit    uint64
	}{
		// The CPU limit is cumulative for a persistent process; each request also has a
		// 30-second interrupt deadline, and the Engine replaces a worker that exits.
		{syscall.RLIMIT_CPU, 300},
		{syscall.RLIMIT_NOFILE, 16},
	}
	for _, limit := range limits {
		// Failure to install any cap means the worker cannot safely evaluate authored code.
		if err := syscall.Setrlimit(limit.resource, &syscall.Rlimit{Cur: limit.limit, Max: limit.limit}); err != nil {
			return fmt.Errorf("resource %d: %w", limit.resource, err)
		}
	}
	return nil
}
