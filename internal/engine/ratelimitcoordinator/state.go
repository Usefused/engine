package ratelimitcoordinator

import (
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

func newState(request ratelimitpolicy.AcquireRequest, now time.Time) ratelimitpolicy.StateEnvelope {
	state := ratelimitpolicy.StateEnvelope{
		SchemaVersion: ratelimitpolicy.ProviderRateLimitStateSchemaVersion,
		AccountID:     request.AccountID, ServiceVersionID: request.ServiceVersionID,
		UpdatedAt: now,
	}
	state.Policies = make([]ratelimitpolicy.PolicyState, len(request.Policies))
	for index, policy := range request.Policies {
		state.Policies[index] = initialPolicyState(policy, now)
	}
	return state
}

func initialPolicyState(policy ratelimitpolicy.ResolvedPolicy, now time.Time) ratelimitpolicy.PolicyState {
	state := ratelimitpolicy.PolicyState{
		Name: policy.Name, ScopeKind: policy.ScopeKind, ScopeID: uuid.MustParse(policy.ScopeID),
		ConfigHash: policy.ConfigHash, Algorithm: policy.Algorithm,
	}
	if policy.Algorithm == "fixed_window" {
		state.FixedWindowStartedAt = timePointer(now)
		return state
	}
	state.Tokens = int64Pointer(policy.Capacity)
	state.TokenRefilledAt = timePointer(now)
	return state
}

func applyAcquisition(state *ratelimitpolicy.StateEnvelope, request ratelimitpolicy.AcquireRequest, now time.Time) (ratelimitpolicy.Decision, bool) {
	changed := reconcileState(state, request, now)
	effectiveNow := monotonicWallTime(now, state.UpdatedAt)
	allowed := cooldownAllows(state.CooldownUntil, effectiveNow)
	retryAt := state.CooldownUntil
	for index, policy := range request.Policies {
		ready, policyRetryAt, policyChanged := preparePolicy(&state.Policies[index], policy, effectiveNow)
		changed = changed || policyChanged
		allowed = allowed && ready
		if !ready {
			retryAt = laterTime(retryAt, policyRetryAt)
		}
	}
	if allowed {
		consumePolicies(state.Policies, request.Policies)
		changed = true
	}
	if changed {
		state.Sequence++
		state.UpdatedAt = effectiveNow
	}
	return ratelimitpolicy.Decision{Allowed: allowed, RetryAfter: retryDuration(retryAt, effectiveNow)}, changed
}

func reconcileState(state *ratelimitpolicy.StateEnvelope, request ratelimitpolicy.AcquireRequest, now time.Time) bool {
	if !sameEnvelopeIdentity(*state, request) {
		reset := newState(request, now)
		reset.ControlEpoch = state.ControlEpoch + 1
		*state = reset
		return true
	}
	changed := false
	for index, policy := range request.Policies {
		current := state.Policies[index]
		if current.ConfigHash == policy.ConfigHash && current.Algorithm == policy.Algorithm {
			continue
		}
		state.Policies[index] = initialPolicyState(policy, now)
		changed = true
	}
	if changed {
		state.ControlEpoch++
	}
	return changed
}

func sameEnvelopeIdentity(state ratelimitpolicy.StateEnvelope, request ratelimitpolicy.AcquireRequest) bool {
	if state.AccountID != request.AccountID || state.ServiceVersionID != request.ServiceVersionID || len(state.Policies) != len(request.Policies) {
		return false
	}
	for index, policy := range request.Policies {
		current := state.Policies[index]
		if current.Name != policy.Name || current.ScopeKind != policy.ScopeKind || current.ScopeID.String() != policy.ScopeID {
			return false
		}
	}
	return true
}

func preparePolicy(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) (bool, *time.Time, bool) {
	if policy.Algorithm == "fixed_window" {
		return prepareFixedWindow(state, policy, now)
	}
	return prepareTokenBucket(state, policy, now)
}

func prepareFixedWindow(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) (bool, *time.Time, bool) {
	duration := time.Duration(policy.DurationMS) * time.Millisecond
	changed := false
	if state.FixedWindowStartedAt == nil || !now.Before(state.FixedWindowStartedAt.Add(duration)) {
		state.FixedWindowStartedAt = timePointer(now)
		state.FixedWindowUsed = 0
		changed = true
	}
	allowed := policy.Cost <= policy.Limit-state.FixedWindowUsed
	if policy.Cost > policy.Limit {
		return false, nil, changed
	}
	retryAt := state.FixedWindowStartedAt.Add(duration)
	return allowed, &retryAt, changed
}

func prepareTokenBucket(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) (bool, *time.Time, bool) {
	if state.Tokens == nil || state.TokenRefilledAt == nil {
		*state = initialPolicyState(policy, now)
	}
	interval := time.Duration(policy.RefillIntervalMS) * time.Millisecond
	changed := refillTokens(state, policy, now, interval)
	allowed := policy.Cost <= *state.Tokens
	if allowed || policy.Cost > policy.Capacity {
		return allowed, nil, changed
	}
	missing := policy.Cost - *state.Tokens
	steps := (missing + policy.RefillUnits - 1) / policy.RefillUnits
	retryAt := state.TokenRefilledAt.Add(time.Duration(steps) * interval)
	return false, &retryAt, changed
}

func refillTokens(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time, interval time.Duration) bool {
	elapsed := now.Sub(*state.TokenRefilledAt)
	if elapsed < interval {
		return false
	}
	steps := int64(elapsed / interval)
	available := *state.Tokens + minProduct(steps, policy.RefillUnits, policy.Capacity)
	if available > policy.Capacity {
		available = policy.Capacity
	}
	*state.Tokens = available
	advanced := time.Duration(steps) * interval
	refilledAt := state.TokenRefilledAt.Add(advanced)
	state.TokenRefilledAt = &refilledAt
	return true
}

func minProduct(left, right, maximum int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left > maximum/right {
		return maximum
	}
	return left * right
}

func consumePolicies(states []ratelimitpolicy.PolicyState, policies []ratelimitpolicy.ResolvedPolicy) {
	for index, policy := range policies {
		if policy.Algorithm == "fixed_window" {
			states[index].FixedWindowUsed += policy.Cost
			continue
		}
		*states[index].Tokens -= policy.Cost
	}
}

func conservativeRecovery(state ratelimitpolicy.StateEnvelope, request ratelimitpolicy.AcquireRequest, now time.Time) ratelimitpolicy.StateEnvelope {
	if !sameEnvelopeIdentity(state, request) {
		return newState(request, now)
	}
	state.CooldownUntil = laterTime(state.CooldownUntil, nil)
	state.ControlEpoch++
	state.Sequence++
	state.UpdatedAt = now
	for index, policy := range request.Policies {
		if state.Policies[index].ConfigHash != policy.ConfigHash || state.Policies[index].Algorithm != policy.Algorithm {
			state.Policies[index] = initialPolicyState(policy, now)
			continue
		}
		makeRecoveredPolicyConservative(&state.Policies[index], policy, now)
	}
	return state
}

func makeRecoveredPolicyConservative(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) {
	if policy.Algorithm == "fixed_window" {
		state.FixedWindowUsed = policy.Limit
		// The projection may predate an unobserved rollover. Starting a fresh,
		// empty window is the only recovery choice that cannot double-grant a
		// provider allowance after a total JetStream state loss.
		state.FixedWindowStartedAt = timePointer(now)
		return
	}
	state.Tokens = int64Pointer(0)
	state.TokenRefilledAt = timePointer(now)
}

func validateState(state ratelimitpolicy.StateEnvelope) error {
	return state.Validate()
}

func cooldownAllows(until *time.Time, now time.Time) bool {
	return until == nil || !until.After(now)
}

func retryDuration(retryAt *time.Time, now time.Time) time.Duration {
	if retryAt == nil || !retryAt.After(now) {
		return 0
	}
	return retryAt.Sub(now)
}

func laterTime(left, right *time.Time) *time.Time {
	if left == nil {
		return cloneTime(right)
	}
	if right != nil && right.After(*left) {
		return cloneTime(right)
	}
	return cloneTime(left)
}

func monotonicWallTime(now, previous time.Time) time.Time {
	if previous.After(now) {
		return previous
	}
	return now
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func int64Pointer(value int64) *int64 {
	return &value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
