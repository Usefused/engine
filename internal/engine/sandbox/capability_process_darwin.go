//go:build darwin

package sandbox

import (
	"context"
	"os/exec"
	"strconv"
)

// capabilityWorkerCommand applies a deny-default macOS sandbox profile before executing the child.
func capabilityWorkerCommand(ctx context.Context) (*exec.Cmd, func(), error) {
	executable, err := capabilityExecutable()
	// The sandbox profile must name the exact packaged worker binary.
	if err != nil {
		return nil, nil, err
	}
	// Permit only the packaged worker launch and Apple loader bootstrap; user files, writes, and network stay denied.
	profile := `(version 1)(deny default)(import "dyld-support.sb")(allow process-exec (literal ` + strconv.Quote(executable) + `))(allow file-read* (literal ` + strconv.Quote(executable) + `) (subpath "/System") (subpath "/usr/lib"))(allow sysctl-read)(allow process-info*)`
	command := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, executable)
	command.Env = capabilityChildEnvironment()
	return command, func() {}, nil
}
