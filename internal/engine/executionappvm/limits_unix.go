//go:build linux || darwin

package executionappvm

import (
	"fmt"
	"syscall"
)

// limitCapabilityWorker bounds process memory, CPU time, and file descriptors independently of Goja limits.
func limitCapabilityWorker() error {
	limits := []struct {
		resource int
		limit    uint64
	}{
		// A 755 KiB bundle failed under a 1 GiB virtual cap but passed under 2 GiB with 48 MiB peak RSS;
		// Go on Linux arm64 needs address reservations beyond physical usage, so retain this OS bound.
		{syscall.RLIMIT_AS, 2 << 30},
		// Linux charges anonymous worker memory to RLIMIT_DATA, giving a smaller physical-growth
		// ceiling without rejecting the Go runtime's mostly untouched virtual reservations.
		{syscall.RLIMIT_DATA, 128 << 20},
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
