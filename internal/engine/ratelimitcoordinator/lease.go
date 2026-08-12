package ratelimitcoordinator

import (
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
)

const (
	leaseShardCount  = 64
	leaseTargetCalls = int64(8)
)

type leaseRegistry struct {
	shards [leaseShardCount]leaseShard
}

type leaseShard struct {
	mu      sync.Mutex
	entries map[string]*leaseEntry
}

type leaseEntry struct {
	mu            sync.Mutex
	refs          int
	observedEpoch uint64
	lease         *localLease
}

type heldLeaseEntry struct {
	key   string
	shard *leaseShard
	entry *leaseEntry
}

type localLease struct {
	controlEpoch uint64
	expiresAt    time.Time
	policies     []leasedPolicy
}

type leasedPolicy struct {
	configHash string
	remaining  int64
}

func (r *leaseRegistry) hold(key string) heldLeaseEntry {
	shard := &r.shards[leaseShardIndex(key)]
	shard.mu.Lock()
	if shard.entries == nil {
		shard.entries = make(map[string]*leaseEntry)
	}
	entry := shard.entries[key]
	if entry == nil {
		entry = &leaseEntry{}
		shard.entries[key] = entry
	}
	entry.refs++
	shard.mu.Unlock()
	entry.mu.Lock()
	return heldLeaseEntry{key: key, shard: shard, entry: entry}
}

func (r *leaseRegistry) observeEpoch(key string, epoch uint64) {
	shard := &r.shards[leaseShardIndex(key)]
	shard.mu.Lock()
	entry := shard.entries[key]
	if entry != nil {
		entry.refs++
	}
	shard.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	if epoch > entry.observedEpoch {
		entry.observedEpoch = epoch
	}
	if entry.lease != nil && epoch > entry.lease.controlEpoch {
		entry.lease = nil
	}
	entry.mu.Unlock()
	releaseLeaseReference(key, shard, entry)
}

func (h heldLeaseEntry) release() {
	h.entry.mu.Unlock()
	releaseLeaseReference(h.key, h.shard, h.entry)
}

func releaseLeaseReference(key string, shard *leaseShard, entry *leaseEntry) {
	shard.mu.Lock()
	defer shard.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && entry.lease == nil {
		delete(shard.entries, key)
	}
}

func (h heldLeaseEntry) consume(policies []ratelimitpolicy.ResolvedPolicy, now time.Time) (ratelimitpolicy.Decision, bool) {
	lease := h.entry.lease
	if !usableLease(lease, policies, now, h.entry.observedEpoch) {
		h.entry.lease = nil
		return ratelimitpolicy.Decision{}, false
	}
	for index, policy := range policies {
		lease.policies[index].remaining -= policy.Cost
	}
	if leaseExhausted(lease) {
		h.entry.lease = nil
	}
	return ratelimitpolicy.Decision{Allowed: true, LocalLease: true}, true
}

func (h heldLeaseEntry) installLease(actual, reserved []ratelimitpolicy.ResolvedPolicy, state ratelimitpolicy.StateEnvelope, now time.Time) {
	if state.ControlEpoch < h.entry.observedEpoch {
		return
	}
	expiresAt, ok := leaseExpiry(state, reserved)
	if !ok || !expiresAt.After(now) {
		return
	}
	lease := &localLease{controlEpoch: state.ControlEpoch, expiresAt: expiresAt, policies: make([]leasedPolicy, len(actual))}
	for index := range actual {
		lease.policies[index] = leasedPolicy{configHash: actual[index].ConfigHash, remaining: reserved[index].Cost - actual[index].Cost}
	}
	if !leaseExhausted(lease) {
		h.entry.lease = lease
	}
}

func (h heldLeaseEntry) clearLease() {
	h.entry.lease = nil
}

func leaseReservation(request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.AcquireRequest, bool) {
	calls := leaseTargetCalls
	for _, policy := range request.Policies {
		if policy.Cost == 0 {
			continue
		}
		// Rolling history and in-flight concurrency must stay in the shared CAS
		// document; local reservation would make either signal incomplete.
		if policy.Algorithm == "rolling_window" || policy.Algorithm == "concurrency" || policy.Mode == "observe" {
			return request, false
		}
		if possible := policyLeaseCalls(policy); possible < calls {
			calls = possible
		}
	}
	if calls <= 1 {
		return request, false
	}
	reserved := request
	reserved.Policies = append([]ratelimitpolicy.ResolvedPolicy(nil), request.Policies...)
	for index := range reserved.Policies {
		if reserved.Policies[index].Cost > 0 {
			reserved.Policies[index].Cost *= calls
		}
	}
	return reserved, true
}

func policyLeaseCalls(policy ratelimitpolicy.ResolvedPolicy) int64 {
	maximum := policy.Limit
	if policy.Algorithm == "token_bucket" {
		maximum = policy.Capacity
	}
	return maximum / policy.Cost
}

func usableLease(lease *localLease, policies []ratelimitpolicy.ResolvedPolicy, now time.Time, observedEpoch uint64) bool {
	if lease == nil || !now.Before(lease.expiresAt) || lease.controlEpoch < observedEpoch || len(lease.policies) != len(policies) {
		return false
	}
	for index, policy := range policies {
		if policy.Cost > 0 && (lease.policies[index].configHash != policy.ConfigHash || lease.policies[index].remaining < policy.Cost) {
			return false
		}
	}
	return true
}

func leaseExhausted(lease *localLease) bool {
	for _, policy := range lease.policies {
		if policy.remaining <= 0 {
			return true
		}
	}
	return false
}

func leaseExpiry(state ratelimitpolicy.StateEnvelope, policies []ratelimitpolicy.ResolvedPolicy) (time.Time, bool) {
	var expiry time.Time
	for index, policy := range policies {
		candidate, ok := policyLeaseExpiry(state.Policies[index], policy)
		if !ok {
			return time.Time{}, false
		}
		if expiry.IsZero() || candidate.Before(expiry) {
			expiry = candidate
		}
	}
	return expiry, !expiry.IsZero()
}

func policyLeaseExpiry(state ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy) (time.Time, bool) {
	if policy.Algorithm == "fixed_window" && state.FixedWindowStartedAt != nil {
		return state.FixedWindowStartedAt.Add(time.Duration(policy.DurationMs) * time.Millisecond), true
	}
	if policy.Algorithm == "token_bucket" && state.TokenRefilledAt != nil {
		return state.TokenRefilledAt.Add(time.Duration(policy.RefillIntervalMs) * time.Millisecond), true
	}
	return time.Time{}, false
}

func leaseShardIndex(key string) int {
	if len(key) == 0 {
		return 0
	}
	return int(key[len(key)-1]) % leaseShardCount
}
