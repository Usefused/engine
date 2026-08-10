package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var ErrProviderRateLimited = errors.New("provider rate limit exceeded")

type providerRateLimitIdentity struct {
	AccountID    uuid.UUID
	BucketID     uuid.UUID
	ConnectionID uuid.UUID
}

type providerRateLimitIdentityKey struct{}

// WithProviderRateLimitIdentity attaches only Engine-resolved scope identity.
// Provider request parameters cannot choose these quota boundaries.
func WithProviderRateLimitIdentity(ctx context.Context, accountID, bucketID, connectionID uuid.UUID) context.Context {
	return context.WithValue(ctx, providerRateLimitIdentityKey{}, providerRateLimitIdentity{
		AccountID: accountID, BucketID: bucketID, ConnectionID: connectionID,
	})
}

func (d *Dispatcher) awaitProviderRateLimit(ctx context.Context, srv *models.Service, obj *models.IntegrationObject) (ratelimitpolicy.Decision, error) {
	if srv.RateLimit == nil {
		return ratelimitpolicy.Decision{Allowed: true}, nil
	}
	if d.rateLimits == nil {
		return ratelimitpolicy.Decision{}, errors.New("provider rate-limit store is unavailable")
	}
	request, err := providerRateLimitRequest(ctx, srv, obj)
	if err != nil {
		return ratelimitpolicy.Decision{}, err
	}
	waited := false
	waitDeadline := providerRateLimitWaitDeadline(srv.RateLimit)
	for {
		decision, acquireErr := d.acquireProviderRateLimit(ctx, request)
		if acquireErr != nil {
			RecordRateLimitSummary(ctx, rateLimitSummary(decision, "acquisition_failed"))
			return decision, acquireErr
		}
		if decision.Allowed {
			RecordRateLimitSummary(ctx, rateLimitSummary(decision, allowedRateLimitRetryOutcome(waited)))
			return decision, nil
		}
		delay, wait := boundedProviderRateLimitDelay(srv.RateLimit, decision, waitDeadline)
		if !wait {
			RecordRateLimitSummary(ctx, rateLimitSummary(decision, "rejected"))
			return decision, ErrProviderRateLimited
		}
		waited = true
		if err := waitForRetry(ctx, delay); err != nil {
			RecordRateLimitSummary(ctx, rateLimitSummary(decision, "cancelled"))
			return decision, err
		}
	}
}

func (d *Dispatcher) acquireProviderRateLimit(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	acquireCtx, span := otel.Tracer("engine").Start(ctx, "engine.provider_rate_limit.acquire")
	defer span.End()
	started := time.Now()
	decision, err := d.rateLimits.AcquireProviderRateLimit(acquireCtx, request)
	AddExecutionTiming(ctx, "rate_limit_acquire", time.Since(started))
	decision = attachRateLimitAggregates(decision, request.Policies)
	recordProviderRateLimitDecision(span, decision, err)
	return decision, err
}

func allowedRateLimitRetryOutcome(waited bool) string {
	if waited {
		return "waited"
	}
	return "none"
}

func boundedProviderRateLimitDelay(config *ratelimitpolicy.Config, decision ratelimitpolicy.Decision, deadline time.Time) (time.Duration, bool) {
	delay, wait := providerRateLimitDelay(config, decision)
	if !wait {
		return 0, false
	}
	return boundedRateLimitWait(delay, deadline)
}

func providerRateLimitWaitDeadline(config *ratelimitpolicy.Config) time.Time {
	if config.RetryAfter == nil {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(config.RetryAfter.MaxDelayMS) * time.Millisecond)
}

func boundedRateLimitWait(delay time.Duration, deadline time.Time) (time.Duration, bool) {
	remaining := time.Until(deadline)
	if deadline.IsZero() || remaining <= 0 {
		return 0, false
	}
	if delay > remaining {
		delay = remaining
	}
	return delay, delay > 0
}

func rateLimitSummary(decision ratelimitpolicy.Decision, retryOutcome string) RateLimitExecutionSummary {
	outcome := "denied"
	if decision.Allowed {
		outcome = "allowed"
	}
	units := make([]string, 0, len(decision.UnitTotals))
	for unit := range decision.UnitTotals {
		units = append(units, unit)
	}
	sort.Strings(units)
	totals := make([]int64, len(units))
	for i, unit := range units {
		totals[i] = decision.UnitTotals[unit]
	}
	if !decision.Allowed {
		units = nil
		totals = nil
	}
	return RateLimitExecutionSummary{
		Decision: outcome, PolicyCount: decision.PolicyCount, ScopeKinds: decision.ScopeKinds,
		Units: units, UnitTotals: totals, RetryOutcome: retryOutcome,
	}
}

func providerRateLimitRequest(ctx context.Context, srv *models.Service, obj *models.IntegrationObject) (ratelimitpolicy.AcquireRequest, error) {
	if err := srv.RateLimit.Validate(); err != nil {
		return ratelimitpolicy.AcquireRequest{}, err
	}
	identity, ok := ctx.Value(providerRateLimitIdentityKey{}).(providerRateLimitIdentity)
	if !ok || identity.AccountID == uuid.Nil || srv.ServiceVersionID == uuid.Nil {
		return ratelimitpolicy.AcquireRequest{}, errors.New("provider rate-limit identity is incomplete")
	}
	policies := make([]ratelimitpolicy.ResolvedPolicy, 0, len(srv.RateLimit.Policies))
	for _, policy := range srv.RateLimit.Policies {
		resolved, err := resolveProviderRateLimitPolicy(policy, obj.StableKey, srv.ServiceVersionID, identity)
		if err != nil {
			return ratelimitpolicy.AcquireRequest{}, err
		}
		policies = append(policies, resolved)
	}
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Name != policies[j].Name {
			return policies[i].Name < policies[j].Name
		}
		return policies[i].ScopeID < policies[j].ScopeID
	})
	return ratelimitpolicy.AcquireRequest{AccountID: identity.AccountID, ServiceVersionID: srv.ServiceVersionID, Policies: policies}, nil
}

func resolveProviderRateLimitPolicy(policy ratelimitpolicy.Policy, stableKey string, serviceVersionID uuid.UUID, identity providerRateLimitIdentity) (ratelimitpolicy.ResolvedPolicy, error) {
	scopeID := serviceVersionID
	if policy.Scope == "connection" {
		scopeID = identity.ConnectionID
		if scopeID == uuid.Nil {
			scopeID = identity.BucketID
		}
	}
	if scopeID == uuid.Nil {
		return ratelimitpolicy.ResolvedPolicy{}, errors.New("provider rate-limit scope is incomplete")
	}
	resolved := ratelimitpolicy.ResolvedPolicy{
		Name: policy.Name, Unit: policy.Unit, ScopeKind: policy.Scope, ScopeID: scopeID.String(),
		Cost: policy.Cost(stableKey), Algorithm: policy.Algorithm,
	}
	if policy.FixedWindow != nil {
		resolved.Limit = policy.FixedWindow.Limit
		resolved.DurationMS = policy.FixedWindow.DurationMS
	}
	if policy.TokenBucket != nil {
		resolved.Capacity = policy.TokenBucket.Capacity
		resolved.RefillUnits = policy.TokenBucket.RefillUnits
		resolved.RefillIntervalMS = policy.TokenBucket.RefillIntervalMS
	}
	resolved.ConfigHash = providerRateLimitConfigHash(resolved)
	return resolved, nil
}

func providerRateLimitConfigHash(policy ratelimitpolicy.ResolvedPolicy) string {
	config := struct {
		Algorithm        string `json:"algorithm"`
		Limit            int64  `json:"limit"`
		DurationMS       int64  `json:"duration_ms"`
		Capacity         int64  `json:"capacity"`
		RefillUnits      int64  `json:"refill_units"`
		RefillIntervalMS int64  `json:"refill_interval_ms"`
	}{policy.Algorithm, policy.Limit, policy.DurationMS, policy.Capacity, policy.RefillUnits, policy.RefillIntervalMS}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func providerRateLimitDelay(config *ratelimitpolicy.Config, decision ratelimitpolicy.Decision) (time.Duration, bool) {
	if config.RetryAfter == nil || decision.RetryAfter <= 0 {
		return 0, false
	}
	maximum := time.Duration(config.RetryAfter.MaxDelayMS) * time.Millisecond
	if decision.RetryAfter > maximum {
		return maximum, true
	}
	return decision.RetryAfter, true
}

func attachRateLimitAggregates(decision ratelimitpolicy.Decision, policies []ratelimitpolicy.ResolvedPolicy) ratelimitpolicy.Decision {
	decision.PolicyCount = int64(len(policies))
	decision.UnitTotals = make(map[string]int64)
	scopes := make(map[string]struct{})
	for _, policy := range policies {
		decision.UnitTotals[policy.Unit] += policy.Cost
		scopes[policy.ScopeKind] = struct{}{}
	}
	for scope := range scopes {
		decision.ScopeKinds = append(decision.ScopeKinds, scope)
	}
	sort.Strings(decision.ScopeKinds)
	return decision
}

func recordProviderRateLimitDecision(span trace.Span, decision ratelimitpolicy.Decision, err error) {
	outcome := "denied"
	if decision.Allowed {
		outcome = "allowed"
	}
	summary := rateLimitSummary(decision, "none")
	span.SetAttributes(
		attribute.String("rate_limit.decision", outcome),
		attribute.Int64("rate_limit.policy_count", decision.PolicyCount),
		attribute.StringSlice("rate_limit.scope_kinds", decision.ScopeKinds),
		attribute.StringSlice("rate_limit.units", summary.Units),
		attribute.Int64Slice("rate_limit.unit_totals", summary.UnitTotals),
		attribute.Int64("rate_limit.retry_after_ms", decision.RetryAfter.Milliseconds()),
		attribute.String("rate_limit.coordinator", "jetstream_kv"),
		attribute.Int64("rate_limit.coordination_attempts", decision.CoordinationAttempts),
		attribute.Bool("rate_limit.local_lease", decision.LocalLease),
	)
	if err != nil {
		span.SetStatus(codes.Error, "acquisition_failed")
	}
}
