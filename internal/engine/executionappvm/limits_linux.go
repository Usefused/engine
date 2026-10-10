//go:build linux

package executionappvm

import (
	"fmt"
	"syscall"
)

// limitCapabilityMemory preserves Linux's separate virtual reservation and anonymous-memory caps.
func limitCapabilityMemory() error {
	// Go reserves more virtual space than its resident heap; DATA independently bounds anonymous growth.
	for _, bound := range []struct {
		resource int
		size     uint64
	}{{syscall.RLIMIT_AS, 2 << 30}, {syscall.RLIMIT_DATA, 128 << 20}} {
		// Failure to install either bound must reject execution rather than silently reduce isolation.
		if err := syscall.Setrlimit(bound.resource, &syscall.Rlimit{Cur: bound.size, Max: bound.size}); err != nil {
			return fmt.Errorf("resource %d: %w", bound.resource, err)
		}
	}
	return nil
}
