//go:build !windows

package agentbundle

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
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
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		// Only the lock owner may publish an installation; other failures must release the handle.
		if err == nil {
			return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
		}
		// Only the lock owner may publish an installation; other failures must release the handle.
		if !errors.Is(err, unix.EWOULDBLOCK) {
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
