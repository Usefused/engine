//go:build darwin

package executionappvm

// confineWorker relies on the parent-applied deny-default sandbox-exec profile on Darwin.
func confineWorker() error { return nil }
