//go:build !darwin && !linux

package sandbox

import (
	"context"
	"errors"
	"os/exec"
)

// capabilityWorkerCommand rejects hosts without an implemented OS isolation boundary.
func capabilityWorkerCommand(context.Context) (*exec.Cmd, func(), error) {
	return nil, nil, errors.New("capability worker isolation is unsupported")
}
