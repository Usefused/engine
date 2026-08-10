package ratelimitcoordinator

import (
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
)

func applySynchronization(state *ratelimitpolicy.StateEnvelope, request ratelimitpolicy.SyncRequest, now time.Time) bool {
	changed := applyCooldown(state, request.CooldownUntil)
	byIdentity := observationIndex(request.Observations)
	for index := range state.Policies {
		observation, ok := byIdentity[identityOfState(state.Policies[index])]
		if !ok {
			continue
		}
		if applyObservation(&state.Policies[index], observation) {
			changed = true
		}
	}
	if changed {
		state.ControlEpoch++
		state.Sequence++
		state.UpdatedAt = monotonicWallTime(now, state.UpdatedAt)
	}
	return changed
}

func applyCooldown(state *ratelimitpolicy.StateEnvelope, proposed *time.Time) bool {
	if proposed == nil || (state.CooldownUntil != nil && !proposed.After(*state.CooldownUntil)) {
		return false
	}
	state.CooldownUntil = cloneTime(proposed)
	return true
}

func observationIndex(observations []ratelimitpolicy.ResponseObservation) map[policyIdentity]ratelimitpolicy.ResponseObservation {
	indexed := make(map[policyIdentity]ratelimitpolicy.ResponseObservation, len(observations))
	for _, observation := range observations {
		identity := policyIdentity{observation.PolicyName, observation.ScopeKind, observation.ScopeID}
		indexed[identity] = observation
	}
	return indexed
}

func identityOfState(state ratelimitpolicy.PolicyState) policyIdentity {
	return policyIdentity{state.Name, state.ScopeKind, state.ScopeID}
}

func applyObservation(state *ratelimitpolicy.PolicyState, observation ratelimitpolicy.ResponseObservation) bool {
	if observation.Algorithm == "fixed_window" {
		return applyFixedWindowObservation(state, observation)
	}
	if observation.Algorithm == "token_bucket" {
		return applyTokenObservation(state, observation)
	}
	return false
}

func applyFixedWindowObservation(state *ratelimitpolicy.PolicyState, observation ratelimitpolicy.ResponseObservation) bool {
	changed := false
	if observation.ResetAt != nil {
		startedAt := observation.ResetAt.Add(-time.Duration(observation.DurationMS) * time.Millisecond)
		if state.FixedWindowStartedAt == nil || startedAt.After(*state.FixedWindowStartedAt) {
			state.FixedWindowStartedAt = &startedAt
			changed = true
		}
	}
	if observation.Remaining == nil {
		return changed
	}
	remaining := boundedRemaining(*observation.Remaining, observation)
	used := observation.LocalLimit - remaining
	if used > state.FixedWindowUsed {
		state.FixedWindowUsed = used
		changed = true
	}
	return changed
}

func applyTokenObservation(state *ratelimitpolicy.PolicyState, observation ratelimitpolicy.ResponseObservation) bool {
	if observation.Remaining == nil || state.Tokens == nil {
		return false
	}
	remaining := boundedRemaining(*observation.Remaining, observation)
	if remaining >= *state.Tokens {
		return false
	}
	*state.Tokens = remaining
	return true
}

func boundedRemaining(remaining int64, observation ratelimitpolicy.ResponseObservation) int64 {
	if remaining > observation.LocalLimit {
		remaining = observation.LocalLimit
	}
	if observation.Limit != nil && remaining > *observation.Limit {
		remaining = *observation.Limit
	}
	return remaining
}
