//go:build darwin

package executionappvm

import (
	"fmt"
	"math"
	"os"
	"syscall"
	"unsafe"
)

const capabilityDarwinMemoryGrowth = uint64(128 << 20)

// limitCapabilityMemory caps new mappings beyond macOS's preloaded shared cache and Go reservations.
func limitCapabilityMemory() error {
	baseline, err := capabilityVirtualSize()
	// An unreadable or invalid startup footprint must never turn into an unbounded worker.
	if err != nil {
		return err
	}
	limit := baseline + capabilityDarwinMemoryGrowth
	// Darwin counts the shared cache in both AS and DATA, so a fixed Linux DATA cap is invalid here.
	// The hard AS cap bounds mapping growth; it is not a claim of a 128 MiB resident-memory limit.
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: limit, Max: limit}); err != nil {
		return fmt.Errorf("worker address-space limit: %w", err)
	}
	return nil
}

// capabilityVirtualSize reads only this worker's native proc_taskinfo before accepting authored input.
func capabilityVirtualSize() (uint64, error) {
	// Darwin sys/proc_info.h defines PROC_PIDTASKINFO as a 96-byte record, starting with virtual size.
	var info [12]uint64
	const procInfoCallPIDInfo = 2
	const procPIDTaskInfo = 4
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(os.Getpid()), procPIDTaskInfo, 0, uintptr(unsafe.Pointer(&info[0])), unsafe.Sizeof(info))
	// Validate the complete native record and arithmetic before deriving a kernel-enforced hard cap.
	if errno != 0 || n != unsafe.Sizeof(info) || info[0] == 0 || info[0] > math.MaxInt64-capabilityDarwinMemoryGrowth {
		return 0, fmt.Errorf("worker memory footprint unavailable: %v", errno)
	}
	return info[0], nil
}
