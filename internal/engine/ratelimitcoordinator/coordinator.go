package ratelimitcoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/nats-io/nats.go"
)

const maxCASAttempts = 256

// RecoveryStore is consulted only when JetStream has no live value. Normal
// execution never waits for PostgreSQL.
type RecoveryStore interface {
	LoadProviderRateLimitState(context.Context, ratelimitpolicy.AcquireRequest) (*ratelimitpolicy.StateEnvelope, error)
}

type Coordinator struct {
	kv           nats.KeyValue
	recovery     RecoveryStore
	now          func() time.Time
	leases       leaseRegistry
	watcher      nats.KeyWatcher
	leaseEnabled atomic.Bool
}

func New(kv nats.KeyValue, recovery RecoveryStore) (*Coordinator, error) {
	if kv == nil {
		return nil, errors.New("provider rate-limit JetStream KV is required")
	}
	watcher, err := kv.WatchAll(nats.UpdatesOnly(), nats.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("watch provider rate-limit state: %w", err)
	}
	coordinator := &Coordinator{kv: kv, recovery: recovery, now: time.Now, watcher: watcher}
	coordinator.leaseEnabled.Store(true)
	go coordinator.watchControlEpochs(watcher)
	return coordinator, nil
}

func (c *Coordinator) Close() error {
	if c == nil || c.watcher == nil {
		return nil
	}
	return c.watcher.Stop()
}

func (c *Coordinator) AcquireProviderRateLimit(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	policies, key, err := validateAcquireRequest(request)
	if err != nil {
		return ratelimitpolicy.Decision{}, err
	}
	request.Policies = policies
	held := c.leases.hold(key)
	defer held.release()
	if decision, ok := c.consumeLocalLease(held, request); ok {
		return decision, nil
	}
	return c.acquireReservation(ctx, held, key, request)
}

func (c *Coordinator) ReleaseProviderRateLimit(ctx context.Context, request ratelimitpolicy.ReleaseRequest) error {
	policies, key, err := validateAcquireRequest(request)
	if err != nil {
		return err
	}
	request.Policies = policies
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = c.releaseAttempt(key, request)
		if err == nil {
			return nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return err
		}
	}
	return errors.New("provider concurrency release exceeded retry budget")
}

func (c *Coordinator) releaseAttempt(key string, request ratelimitpolicy.ReleaseRequest) error {
	entry, err := c.kv.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load provider concurrency state: %w", err)
	}
	state, err := decodeState(entry.Value())
	if err != nil || !applyRelease(&state, request, c.now().UTC()) {
		return err
	}
	return updateState(c.kv, key, state, entry.Revision())
}

func (c *Coordinator) consumeLocalLease(held heldLeaseEntry, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, bool) {
	if c.leaseEnabled.Load() {
		return held.consume(request.Policies, c.now().UTC())
	}
	held.clearLease()
	return ratelimitpolicy.Decision{}, false
}

func (c *Coordinator) acquireReservation(ctx context.Context, held heldLeaseEntry, key string, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	reservation, leased := leaseReservation(request)
	leased = leased && c.leaseEnabled.Load()
	if !leased {
		reservation = request
	}
	decision, state, err := c.acquireCentral(ctx, key, reservation)
	if err != nil {
		return ratelimitpolicy.Decision{}, err
	}
	if leased && !decision.Allowed {
		leased = false
		decision, state, err = c.acquireCentral(ctx, key, request)
	}
	if err != nil || !decision.Allowed || !leased {
		return decision, err
	}
	held.installLease(request.Policies, reservation.Policies, state, c.now().UTC())
	return decision, nil
}

func (c *Coordinator) acquireCentral(ctx context.Context, key string, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, ratelimitpolicy.StateEnvelope, error) {
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return ratelimitpolicy.Decision{}, ratelimitpolicy.StateEnvelope{}, err
		}
		result, err := c.acquireAttempt(ctx, key, request)
		if err == nil {
			result.decision.CoordinationAttempts = int64(attempt + 1)
			return result.decision, result.state, nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return ratelimitpolicy.Decision{}, ratelimitpolicy.StateEnvelope{}, err
		}
	}
	return ratelimitpolicy.Decision{}, ratelimitpolicy.StateEnvelope{}, errors.New("provider rate-limit JetStream contention exceeded retry budget")
}

type acquireResult struct {
	decision ratelimitpolicy.Decision
	state    ratelimitpolicy.StateEnvelope
}

func (c *Coordinator) acquireAttempt(ctx context.Context, key string, request ratelimitpolicy.AcquireRequest) (acquireResult, error) {
	entry, err := c.kv.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return c.createAndAcquire(ctx, key, request)
	}
	if err != nil {
		return acquireResult{}, fmt.Errorf("load provider rate-limit state: %w", err)
	}
	state, err := decodeState(entry.Value())
	if err != nil {
		return acquireResult{}, err
	}
	decision, changed := applyAcquisition(&state, request, c.now().UTC())
	if !changed {
		return acquireResult{decision: decision, state: state}, nil
	}
	if err := updateState(c.kv, key, state, entry.Revision()); err != nil {
		return acquireResult{}, err
	}
	return acquireResult{decision: decision, state: state}, nil
}

func (c *Coordinator) createAndAcquire(ctx context.Context, key string, request ratelimitpolicy.AcquireRequest) (acquireResult, error) {
	state, err := c.recoverOrInitialize(ctx, request)
	if err != nil {
		return acquireResult{}, err
	}
	decision, _ := applyAcquisition(&state, request, c.now().UTC())
	if err := validateState(state); err != nil {
		return acquireResult{}, err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return acquireResult{}, fmt.Errorf("encode provider rate-limit state: %w", err)
	}
	if _, err := c.kv.Create(key, payload); err != nil {
		return acquireResult{}, err
	}
	return acquireResult{decision: decision, state: state}, nil
}

func (c *Coordinator) recoverOrInitialize(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.StateEnvelope, error) {
	now := c.now().UTC()
	if c.recovery == nil {
		return newState(request, now), nil
	}
	recovered, err := c.recovery.LoadProviderRateLimitState(ctx, request)
	if err != nil {
		return ratelimitpolicy.StateEnvelope{}, fmt.Errorf("recover provider rate-limit state: %w", err)
	}
	if recovered == nil {
		return newState(request, now), nil
	}
	return conservativeRecovery(*recovered, request, now), nil
}

func (c *Coordinator) SyncProviderRateLimit(ctx context.Context, request ratelimitpolicy.SyncRequest) error {
	key, err := validateSyncRequest(request)
	if err != nil {
		return err
	}
	held := c.leases.hold(key)
	defer held.release()
	// A provider clamp is authoritative even while JetStream is temporarily
	// unavailable, so this Engine must stop spending its existing lease first.
	held.clearLease()
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.syncAttempt(key, request)
		if err == nil {
			return nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return err
		}
	}
	return errors.New("provider rate-limit synchronization exceeded retry budget")
}

func (c *Coordinator) syncAttempt(key string, request ratelimitpolicy.SyncRequest) error {
	entry, err := c.kv.Get(key)
	if err != nil {
		return fmt.Errorf("load provider rate-limit state for synchronization: %w", err)
	}
	state, err := decodeState(entry.Value())
	if err != nil {
		return err
	}
	// Even a no-op observation is CAS-written. A follower may have served a
	// stale read; confirming the revision prevents a newer refill from escaping
	// a provider clamp that looked redundant against that older value.
	now := c.now().UTC()
	if !applySynchronization(&state, request, now) {
		state.Sequence++
		state.UpdatedAt = monotonicWallTime(now, state.UpdatedAt)
	}
	return updateState(c.kv, key, state, entry.Revision())
}

func (c *Coordinator) watchControlEpochs(watcher nats.KeyWatcher) {
	defer c.leaseEnabled.Store(false)
	for entry := range watcher.Updates() {
		if entry == nil {
			continue
		}
		state, err := decodeState(entry.Value())
		if err != nil {
			continue
		}
		c.leases.observeEpoch(entry.Key(), state.ControlEpoch)
	}
}

func updateState(kv nats.KeyValue, key string, state ratelimitpolicy.StateEnvelope, revision uint64) error {
	if err := validateState(state); err != nil {
		return err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode provider rate-limit state: %w", err)
	}
	if _, err := kv.Update(key, payload, revision); err != nil {
		return err
	}
	return nil
}

func decodeState(payload []byte) (ratelimitpolicy.StateEnvelope, error) {
	var state ratelimitpolicy.StateEnvelope
	if err := json.Unmarshal(payload, &state); err != nil {
		return state, fmt.Errorf("decode provider rate-limit state: %w", err)
	}
	if err := validateState(state); err != nil {
		return state, err
	}
	return state, nil
}
