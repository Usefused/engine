//go:build !darwin && !linux

package executionappvm

import "errors"

// confineWorker rejects systems without an implemented OS isolation boundary.
func confineWorker() error { return errors.New("capability worker isolation is unsupported") }
