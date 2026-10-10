//go:build darwin

package executionappvm

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestDarwinWorkerMemoryBound installs irreversible limits only in a disposable child and proves growth is denied.
func TestDarwinWorkerMemoryBound(t *testing.T) {
	// Parent test processes must remain unrestricted for unrelated package checks.
	if os.Getenv("FUSED_TEST_MEMORY_LIMIT") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestDarwinWorkerMemoryBound$")
		command.Env = append(os.Environ(), "FUSED_TEST_MEMORY_LIMIT=1")
		// A failed child proves startup or memory enforcement is broken, rather than skipping macOS coverage.
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("bounded worker failed: %v: %s", err, output)
		}
		return
	}
	// All production worker caps must install together before authored code would be accepted.
	if err := limitCapabilityWorker(); err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	// A finite hard limit cannot be raised again by the worker after startup.
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &limit); err != nil || limit.Cur != limit.Max || limit.Cur == ^uint64(0)>>1 {
		t.Fatalf("address-space cap was not installed: %+v, %v", limit, err)
	}
	mapped, err := syscall.Mmap(-1, 0, int(2*capabilityDarwinMemoryGrowth), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	// A mapping larger than the remaining growth budget must fail in the kernel before memory is touched.
	if err == nil {
		_ = syscall.Munmap(mapped)
		t.Fatal("allocation escaped the worker memory growth cap")
	}
	// ENOMEM distinguishes a real resource cap from an unrelated malformed syscall.
	if err != syscall.ENOMEM {
		t.Fatalf("unexpected mapping error: %v", err)
	}
}
