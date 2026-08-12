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
	if policy.Algorithm == "rolling_window" || policy.Algorithm == "concurrency" {
		return state
	}
	state.Tokens = int64Pointer(policy.Capacity)
	state.TokenRefilledAt = timePointer(now)
	return state
}

func applyAcquisition(state *ratelimitpolicy.StateEnvelope, request ratelimitpolicy.AcquireRequest, now time.Time) (ratelimitpolicy.Decision, bool) {
	changed := reconcileState(state, request, now)
	effectiveNow := monotonicWallTime(now, state.UpdatedAt)
	evaluation := evaluatePolicies(state, request.Policies, effectiveNow)
	allowed := cooldownAllows(state.CooldownUntil, effectiveNow) && evaluation.allowed
	changed = changed || evaluation.changed
	if allowed {
		consumePolicies(state.Policies, request.Policies, request.PermitID, request.PermitExpiresAt, evaluation.consumable, effectiveNow)
		changed = true
	}
	if changed {
		state.Sequence++
		state.UpdatedAt = effectiveNow
	}
	return ratelimitpolicy.Decision{Allowed: allowed, RetryAfter: retryDuration(laterTime(state.CooldownUntil, evaluation.retryAt), effectiveNow), ObservedDenials: evaluation.observedDenials, ConcurrencyDenied: evaluation.concurrencyDenied}, changed
}

type policyEvaluation struct {
	allowed           bool
	changed           bool
	consumable        []bool
	retryAt           *time.Time
	observedDenials   int64
	concurrencyDenied bool
}

func evaluatePolicies(state *ratelimitpolicy.StateEnvelope, policies []ratelimitpolicy.ResolvedPolicy, now time.Time) policyEvaluation {
	result := policyEvaluation{allowed: true, consumable: make([]bool, len(policies))}
	for index, policy := range policies {
		if policy.Cost == 0 {
			result.consumable[index] = true
			continue
		}
		ready, retryAt, changed := preparePolicy(&state.Policies[index], policy, now)
		result.changed = result.changed || changed
		result.consumable[index] = ready
		if !ready && policy.Mode == string(ratelimitpolicy.ModeObserve) {
			result.observedDenials++
			result.consumable[index] = true
			continue
		}
		result.allowed = result.allowed && ready
		result.concurrencyDenied = result.concurrencyDenied || (!ready && policy.Algorithm == "concurrency")
		if !ready {
			result.retryAt = laterTime(result.retryAt, retryAt)
		}
	}
	return result
}

func applyRelease(state *ratelimitpolicy.StateEnvelope, request ratelimitpolicy.ReleaseRequest, now time.Time) bool {
	if !sameEnvelopeIdentity(*state, request) || request.PermitID == uuid.Nil {
		return false
	}
	changed := false
	for index, policy := range request.Policies {
		if policy.Algorithm != "concurrency" {
			continue
		}
		holder, exists := state.Policies[index].ConcurrencyHolders[request.PermitID.String()]
		if !exists {
			continue
		}
		state.Policies[index].ConcurrencyUsed -= holder.Cost
		delete(state.Policies[index].ConcurrencyHolders, request.PermitID.String())
		changed = true
	}
	if changed {
		state.Sequence++
		state.UpdatedAt = monotonicWallTime(now, state.UpdatedAt)
	}
	return changed
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
	switch policy.Algorithm {
	case "fixed_window":
		return prepareFixedWindow(state, policy, now)
	case "rolling_window":
		return prepareRollingWindow(state, policy, now)
	case "concurrency":
		return prepareConcurrency(state, policy, now)
	default:
		return prepareTokenBucket(state, policy, now)
	}
}

func prepareConcurrency(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) (bool, *time.Time, bool) {
	changed := false
	for id, holder := range state.ConcurrencyHolders {
		if holder.ExpiresAt.After(now) {
			continue
		}
		state.ConcurrencyUsed -= holder.Cost
		delete(state.ConcurrencyHolders, id)
		changed = true
	}
	return policy.Cost <= policy.ConcurrencyLimit-state.ConcurrencyUsed, nil, changed
}

func prepareRollingWindow(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) (bool, *time.Time, bool) {
	width := rollingBucketWidth(policy)
	cutoff := now.Add(-time.Duration(policy.RollingDurationMs)*time.Millisecond - width)
	first := 0
	used := int64(0)
	for first < len(state.RollingUsage) && !state.RollingUsage[first].At.After(cutoff) {
		first++
	}
	for index := first; index < len(state.RollingUsage); index++ {
		used += state.RollingUsage[index].Cost
	}
	changed := first > 0
	state.RollingUsage = state.RollingUsage[first:]
	if policy.Cost <= policy.RollingLimit-used {
		return true, nil, changed
	}
	retryAt := rollingRetryAt(state.RollingUsage, policy, used, width)
	return false, retryAt, changed
}

func rollingRetryAt(usage []ratelimitpolicy.RollingUsage, policy ratelimitpolicy.ResolvedPolicy, used int64, width time.Duration) *time.Time {
	required := used + policy.Cost - policy.RollingLimit
	for _, item := range usage {
		required -= item.Cost
		if required <= 0 {
			when := item.At.Add(time.Duration(policy.RollingDurationMs)*time.Millisecond + width)
			return &when
		}
	}
	return nil
}

func rollingBucketWidth(policy ratelimitpolicy.ResolvedPolicy) time.Duration {
	duration := time.Duration(policy.RollingDurationMs) * time.Millisecond
	return (duration + ratelimitpolicy.MaxRollingStateBuckets - 1) / ratelimitpolicy.MaxRollingStateBuckets
}

func consumeRolling(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) {
	width := rollingBucketWidth(policy)
	bucket := now.Truncate(width)
	last := len(state.RollingUsage) - 1
	if last >= 0 && state.RollingUsage[last].At.Equal(bucket) {
		state.RollingUsage[last].Cost = saturatingAdd(state.RollingUsage[last].Cost, policy.Cost)
		return
	}
	if len(state.RollingUsage) == ratelimitpolicy.MaxRollingStateBuckets {
		// Merging into the newer boundary delays expiry and is therefore
		// conservative while keeping coordination state strictly bounded.
		state.RollingUsage[1].Cost = saturatingAdd(state.RollingUsage[0].Cost, state.RollingUsage[1].Cost)
		copy(state.RollingUsage, state.RollingUsage[1:])
		state.RollingUsage = state.RollingUsage[:len(state.RollingUsage)-1]
	}
	// The conservative extra bucket lifetime means the bounded representation
	// may delay a caller at a boundary, but can never grant more than the
	// provider's rolling allowance.
	state.RollingUsage = append(state.RollingUsage, ratelimitpolicy.RollingUsage{At: bucket, Cost: policy.Cost})
}

func saturatingAdd(left, right int64) int64 {
	if right > ratelimitpolicy.MaxRuntimePolicyValue-left {
		return ratelimitpolicy.MaxRuntimePolicyValue
	}
	return left + right
}

func prepareFixedWindow(state *ratelimitpolicy.PolicyState, policy ratelimitpolicy.ResolvedPolicy, now time.Time) (bool, *time.Time, bool) {
	duration := time.Duration(policy.DurationMs) * time.Millisecond
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
	interval := time.Duration(policy.RefillIntervalMs) * time.Millisecond
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

func consumePolicies(states []ratelimitpolicy.PolicyState, policies []ratelimitpolicy.ResolvedPolicy, permitID uuid.UUID, permitExpiresAt time.Time, consumable []bool, now time.Time) {
	for index, policy := range policies {
		if policy.Cost == 0 || !consumable[index] {
			continue
		}
		if policy.Algorithm == "fixed_window" {
			states[index].FixedWindowUsed = saturatingAdd(states[index].FixedWindowUsed, policy.Cost)
			continue
		}
		if policy.Algorithm == "rolling_window" {
			consumeRolling(&states[index], policy, now)
			continue
		}
		if policy.Algorithm == "concurrency" {
			cost := concurrencyStateCost(policy)
			states[index].ConcurrencyUsed = saturatingAdd(states[index].ConcurrencyUsed, cost)
			if states[index].ConcurrencyHolders == nil {
				states[index].ConcurrencyHolders = make(map[string]ratelimitpolicy.ConcurrencyHolder)
			}
			states[index].ConcurrencyHolders[permitID.String()] = ratelimitpolicy.ConcurrencyHolder{Cost: cost, ExpiresAt: permitExpiresAt}
			continue
		}
		*states[index].Tokens -= min64(*states[index].Tokens, policy.Cost)
	}
}

func concurrencyStateCost(policy ratelimitpolicy.ResolvedPolicy) int64 {
	if policy.Mode == string(ratelimitpolicy.ModeObserve) && policy.Cost > policy.ConcurrencyLimit {
		return policy.ConcurrencyLimit
	}
	return policy.Cost
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
	if policy.Algorithm == "rolling_window" {
		state.RollingUsage = []ratelimitpolicy.RollingUsage{{At: now, Cost: policy.RollingLimit}}
		return
	}
	if policy.Algorithm == "concurrency" {
		state.ConcurrencyUsed = 0
		for id, holder := range state.ConcurrencyHolders {
			if !holder.ExpiresAt.After(now) {
				delete(state.ConcurrencyHolders, id)
				continue
			}
			state.ConcurrencyUsed += holder.Cost
		}
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
