//go:build !linux

package sandbox

import "os/exec"

// applySysProcAttr is a no-op on non-Linux platforms (macOS, Windows).
// Linux-specific process isolation attributes (Pdeathsig, Setpgid) are only
// applied in the production Linux container via sandbox_linux.go.
func applySysProcAttr(_ *exec.Cmd) {}
