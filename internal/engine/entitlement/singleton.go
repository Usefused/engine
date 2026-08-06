package entitlement

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
)

// EngineSuspended is set to true when the Registry heartbeat signals that the
// account is suspended. It is checked by the LicenseEnforcement middleware
// on every HTTP request before dispatching to the route handler.
var EngineSuspended atomic.Bool

// EngineHeartbeatLeaseExpired is set after the Registry heartbeat has been
// unreachable for longer than the Registry-issued stale-after window.
var EngineHeartbeatLeaseExpired atomic.Bool

var lastSuccessfulHeartbeatUnixNano atomic.Int64

func MarkHeartbeatSuccess(at time.Time) {
	lastSuccessfulHeartbeatUnixNano.Store(at.UnixNano())
	EngineHeartbeatLeaseExpired.Store(false)
}

func EvaluateHeartbeatLease(now time.Time, staleAfter time.Duration) bool {
	last := lastSuccessfulHeartbeatUnixNano.Load()
	expired := last == 0 || staleAfter <= 0 || !now.Before(time.Unix(0, last).Add(staleAfter))
	if !expired {
		EngineHeartbeatLeaseExpired.Store(false)
		return false
	}
	return EngineHeartbeatLeaseExpired.CompareAndSwap(false, true)
}

// LiveEntitlement is the thread-safe in-memory singleton seeded from the
// Registry handshake and updated by heartbeat plan-change responses. Runtime
// limit enforcement (sandbox concurrency, max services, bucket limits, SDK
// family whitelist) reads from here to avoid database round-trips on every request.
var LiveEntitlement = &liveEntitlement{}

type liveEntitlement struct {
	mu  sync.RWMutex
	ent models.RuntimeEntitlement
}

// Load returns a Normalized() copy so callers never mutate the shared state.
func (le *liveEntitlement) Load() models.RuntimeEntitlement {
	le.mu.RLock()
	defer le.mu.RUnlock()
	return le.ent.Normalized()
}

// Store replaces the in-memory entitlement. It is called:
//   - during bootstrap after loading/persisting the handshake entitlement
//   - during heartbeat processing when PlanChanged is true (after DB persistence)
func (le *liveEntitlement) Store(e models.RuntimeEntitlement) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.ent = e.Normalized()
	// IsSuspended is handled externally by sendEngineHeartbeat because it is
	// a heartbeat response flag, not a persisted entitlement field.
}

// Reset clears the singleton and the suspended flag. Safe for tests.
func (le *liveEntitlement) Reset() {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.ent = models.RuntimeEntitlement{}
	EngineSuspended.Store(false)
	EngineHeartbeatLeaseExpired.Store(false)
	lastSuccessfulHeartbeatUnixNano.Store(0)
}
