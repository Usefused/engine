//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// capabilityWorkerCommand places every child in fresh user, network, mount, PID, and IPC namespaces.
func capabilityWorkerCommand(ctx context.Context) (*exec.Cmd, func(), error) {
	executable, err := capabilityExecutable()
	// An unresolved Engine binary cannot become a child with guessed executable identity.
	if err != nil {
		return nil, nil, err
	}
	jail, err := os.MkdirTemp("", "fused-capability-jail-")
	// Every invocation needs a fresh empty filesystem root.
	if err != nil {
		return nil, nil, err
	}
	command := exec.CommandContext(ctx, executable)
	command.Env = append(capabilityChildEnvironment(), "FUSED_CAPABILITY_JAIL="+jail)
	command.SysProcAttr = &syscall.SysProcAttr{
		// A resident sandbox must die when its owning Engine exits unexpectedly.
		Pdeathsig:                  syscall.SIGKILL,
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return command, func() { _ = os.Remove(jail) }, nil
}
