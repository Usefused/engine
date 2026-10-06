package agentbundle

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"time"
)

// lockCache serializes cache installation across processes while letting a cancelled waiter leave promptly.
func lockCache(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	// Only the lock owner may publish an installation; other failures must release the handle.
	if err != nil {
		return nil, err
	}
	overlap := &windows.Overlapped{}
	for {
		err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap)
		// Only the lock owner may publish an installation; other failures must release the handle.
		if err == nil {
			return func() { windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlap); f.Close() }, nil
		}
		// Only the lock owner may publish an installation; other failures must release the handle.
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			f.Close()
			return nil, err
		}
		select {
		// Cancellation must release the owned resources rather than leave background work running.
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		// Only the lock owner may publish an installation; other failures must release the handle.
		case <-time.After(100 * time.Millisecond):
		}
	}
}
