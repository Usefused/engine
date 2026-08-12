package ratelimitcoordinator

import (
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

func TestRollingWindowStateRemainsBoundedAndConservative(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	request := v3Request(v3Policy("rolling", "rolling_window", 1))
	request.Policies[0].RollingLimit = 1_000_000
	request.Policies[0].RollingDurationMs = 256_000
	state := newState(request, now)
	for index := 0; index < 2_000; index++ {
		decision, _ := applyAcquisition(&state, request, now.Add(time.Duration(index)*time.Second))
		if !decision.Allowed {
			t.Fatalf("acquisition %d unexpectedly denied", index)
		}
	}
	if len(state.Policies[0].RollingUsage) > ratelimitpolicy.MaxRollingStateBuckets {
		t.Fatalf("rolling buckets = %d", len(state.Policies[0].RollingUsage))
	}
}

func TestObserveConcurrencyTracksWouldDenyAndReleasesIdempotently(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	request := v3Request(v3Policy("parallel", "concurrency", 1))
	request.Policies[0].Mode = string(ratelimitpolicy.ModeObserve)
	request.Policies[0].ConcurrencyLimit = 1
	request.PermitExpiresAt = now.Add(time.Minute)
	state := newState(request, now)
	first, _ := applyAcquisition(&state, request, now)
	secondRequest := request
	secondRequest.PermitID = uuid.New()
	second, _ := applyAcquisition(&state, secondRequest, now)
	if !first.Allowed || !second.Allowed || second.ObservedDenials != 1 || state.Policies[0].ConcurrencyUsed != 2 {
		t.Fatalf("observe decisions/state = %+v %+v %+v", first, second, state.Policies[0])
	}
	if !applyRelease(&state, request, now) || applyRelease(&state, request, now) {
		t.Fatal("concurrency release was not idempotent")
	}
	if !applyRelease(&state, secondRequest, now) || state.Policies[0].ConcurrencyUsed != 0 {
		t.Fatalf("concurrency usage after releases = %d", state.Policies[0].ConcurrencyUsed)
	}
}

func TestConcurrencyRecoveryKeepsOnlyLiveDurableHolders(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	request := v3Request(v3Policy("parallel", "concurrency", 1))
	request.Policies[0].ConcurrencyLimit = 2
	state := newState(request, now)
	state.Policies[0].ConcurrencyHolders = map[string]ratelimitpolicy.ConcurrencyHolder{
		"live": {Cost: 1, ExpiresAt: now.Add(time.Minute)},
		"old":  {Cost: 1, ExpiresAt: now.Add(-time.Second)},
	}
	state.Policies[0].ConcurrencyUsed = 2
	recovered := conservativeRecovery(state, request, now)
	if recovered.Policies[0].ConcurrencyUsed != 1 || len(recovered.Policies[0].ConcurrencyHolders) != 1 {
		t.Fatalf("recovered concurrency state = %+v", recovered.Policies[0])
	}
}

func v3Request(policy ratelimitpolicy.ResolvedPolicy) ratelimitpolicy.AcquireRequest {
	return ratelimitpolicy.AcquireRequest{AccountID: uuid.New(), ServiceVersionID: uuid.New(), PermitID: uuid.New(), Policies: []ratelimitpolicy.ResolvedPolicy{policy}}
}

func v3Policy(name, algorithm string, cost int64) ratelimitpolicy.ResolvedPolicy {
	return ratelimitpolicy.ResolvedPolicy{Name: name, Mode: string(ratelimitpolicy.ModeEnforce), Unit: string(ratelimitpolicy.UnitRequests), ScopeKind: "connection", ScopeID: uuid.NewString(), Cost: cost, Algorithm: algorithm, ConfigHash: name + "-hash"}
}
