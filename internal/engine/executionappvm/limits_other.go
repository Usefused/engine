//go:build !linux && !darwin

package executionappvm

import "errors"

// limitCapabilityWorker rejects platforms without supported process limits instead of running an unbounded worker.
func limitCapabilityWorker() error {
	return errors.New("capability worker resource limits are unsupported")
}
