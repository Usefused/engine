//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
)

// applySysProcAttr configures Linux-specific process attributes on cmd:
//   - Pdeathsig: SIGKILL  — the child Node process is automatically killed if the
//     parent sandbox server process dies, preventing orphaned processes.
//   - Setpgid: true       — places the child in its own process group so that a
//     kill(-pgid) signal terminates all threads/children spawned by the Node process.
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
