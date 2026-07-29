// Package retrypolicy holds the backoff math shared by Engine's two
// independent retry layers: the dispatcher's outbound vendor-call retries
// (internal/engine/dispatcher.go) and the reverse-proxy retry middleware
// (internal/engine/middleware/retry.go). Each layer owns its own retry
// config source (models.RetryConfig vs fusedobject.RetryConfig -- two
// structurally identical but separate types), so this package works on
// primitive values rather than either type, avoiding a dependency between
// the two.
package retrypolicy

import "time"

// BackoffDuration returns how long to sleep before the next retry attempt
// (0-indexed: attempt 0 is the delay before the second overall try).
//
//   - "fixed", or any unrecognized/empty strategy (existing RetryConfig rows
//     predate the exponential strategy, and default to "fixed" behavior):
//     sleeps backoffMs flat before every retry, matching Engine's retry
//     behavior before per-strategy backoff existed.
//   - "exponential": doubles the delay each attempt (backoffMs * 2^attempt),
//     capped at 32x the base delay so a large max_retries can't produce
//     multi-minute sleeps by accident.
//
// backoffMs <= 0 always means no sleep, regardless of strategy.
func BackoffDuration(strategy string, backoffMs, attempt int) time.Duration {
	if backoffMs <= 0 {
		return 0
	}
	if strategy != "exponential" {
		return time.Duration(backoffMs) * time.Millisecond
	}
	const maxShift = 5 // 2^5 = 32x base delay ceiling
	shift := attempt
	if shift > maxShift {
		shift = maxShift
	}
	if shift < 0 {
		shift = 0
	}
	return time.Duration(backoffMs*(1<<shift)) * time.Millisecond
}
