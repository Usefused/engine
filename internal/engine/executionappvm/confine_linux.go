//go:build linux

package executionappvm

import (
	"os"
	"syscall"
)

// confineWorker enters the parent-created empty jail before parsing any authored bundle.
func confineWorker() error {
	jail := os.Getenv("FUSED_CAPABILITY_JAIL")
	// The parent must provide an absolute, fresh jail rather than a caller-controlled path.
	if jail == "" || jail[0] != '/' {
		return syscall.EINVAL
	}
	if err := syscall.Chroot(jail); err != nil {
		// Authored code cannot run with the host filesystem still visible.
		return err
	}
	if err := os.Chdir("/"); err != nil {
		// The worker must not retain a working directory outside the jail.
		return err
	}
	return os.Unsetenv("FUSED_CAPABILITY_JAIL")
}
