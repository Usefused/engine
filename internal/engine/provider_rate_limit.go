package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
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
	Bindings     map[string]string
}

// WithProviderQuotaBindings adds values already resolved from trusted Engine
// connection metadata. Request parameters are intentionally excluded because
// callers must not choose their own quota bucket.
func WithProviderQuotaBindings(ctx context.Context, bindings map[string]string) context.Context {
	identity, _ := ctx.Value(providerRateLimitIdentityKey{}).(providerRateLimitIdentity)
	identity.Bindings = make(map[string]string, len(bindings))
	for key, value := range bindings {
		identity.Bindings[key] = value
	}
	return context.WithValue(ctx, providerRateLimitIdentityKey{}, identity)
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
	decision, _, err := d.awaitProviderRateLimitPermit(ctx, srv, obj)
	return decision, err
}

func (d *Dispatcher) awaitProviderRateLimitPermit(ctx context.Context, srv *models.Service, obj *models.IntegrationObject) (ratelimitpolicy.Decision, ratelimitpolicy.ReleaseRequest, error) {
	if srv.RateLimit == nil {
		return ratelimitpolicy.Decision{Allowed: true}, ratelimitpolicy.ReleaseRequest{}, nil
	}
	request, err := providerRateLimitRequest(ctx, srv, obj)
	if err != nil {
		return ratelimitpolicy.Decision{}, ratelimitpolicy.ReleaseRequest{}, err
	}
	if allRateLimitCostsZero(request.Policies) {
		decision := attachRateLimitAggregates(ratelimitpolicy.Decision{Allowed: true}, request.Policies)
		RecordRateLimitSummary(ctx, rateLimitSummary(decision, "not_applicable"))
		return decision, request, nil
	}
	if d.rateLimits == nil {
		return ratelimitpolicy.Decision{}, request, errors.New("provider rate-limit store is unavailable")
	}
	return d.awaitProviderRateLimitV3(ctx, request)
}

func (d *Dispatcher) awaitProviderRateLimitV3(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, ratelimitpolicy.ReleaseRequest, error) {
	decision, err := d.acquireProviderRateLimit(ctx, request)
	if err != nil {
		RecordRateLimitSummary(ctx, rateLimitSummary(decision, "acquisition_failed"))
		return decision, request, err
	}
	outcome := "rejected"
	if decision.Allowed {
		outcome = "none"
	}
	RecordRateLimitSummary(ctx, rateLimitSummary(decision, outcome))
	if !decision.Allowed {
		return decision, request, ErrProviderRateLimited
	}
	return decision, request, nil
}

func (d *Dispatcher) releaseProviderRateLimit(ctx context.Context, request ratelimitpolicy.ReleaseRequest) {
	if d.rateLimits == nil || !hasConcurrencyPolicy(request.Policies) {
		return
	}
	started := time.Now()
	err := d.rateLimits.ReleaseProviderRateLimit(ctx, request)
	AddExecutionTiming(ctx, "rate_limit_release_ms", time.Since(started))
	_, span := otel.Tracer("engine").Start(ctx, "engine.provider_rate_limit.release")
	defer span.End()
	span.SetAttributes(attribute.Bool("rate_limit.release_success", err == nil))
	if err != nil {
		span.SetStatus(codes.Error, "release_failed")
	}
}

func hasConcurrencyPolicy(policies []ratelimitpolicy.ResolvedPolicy) bool {
	for _, policy := range policies {
		if policy.Algorithm == string(ratelimitpolicy.AlgorithmConcurrency) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) acquireProviderRateLimit(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	acquireCtx, span := otel.Tracer("engine").Start(ctx, "engine.provider_rate_limit.acquire")
	defer span.End()
	started := time.Now()
	decision, err := d.rateLimits.AcquireProviderRateLimit(acquireCtx, request)
	AddExecutionTiming(ctx, "rate_limit_acquire_ms", time.Since(started))
	if hasConcurrencyPolicy(request.Policies) {
		AddExecutionTiming(ctx, "concurrency_wait_ms", 0)
	}
	decision = attachRateLimitAggregates(decision, request.Policies)
	recordProviderRateLimitDecision(span, decision, err)
	return decision, err
}

func rateLimitSummary(decision ratelimitpolicy.Decision, retryOutcome string) RateLimitExecutionSummary {
	outcome := "denied"
	if decision.Allowed {
		outcome = "allowed"
	}
	if decision.Allowed && decision.ObservedDenials > 0 {
		outcome = "would_deny"
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
		Units: units, UnitTotals: totals, RetryOutcome: retryOutcome, ObservedDenials: decision.ObservedDenials,
	}
}

func providerRateLimitRequest(ctx context.Context, srv *models.Service, obj *models.IntegrationObject) (ratelimitpolicy.AcquireRequest, error) {
	if err := srv.RateLimit.Validate(); err != nil {
		return ratelimitpolicy.AcquireRequest{}, err
	}
	identity, ok := ctx.Value(providerRateLimitIdentityKey{}).(providerRateLimitIdentity)
	if !validProviderRateLimitIdentity(identity, ok, srv.ServiceVersionID) {
		return ratelimitpolicy.AcquireRequest{}, errors.New("provider rate-limit identity is incomplete")
	}
	policies, err := resolveProviderRateLimitPolicies(srv.RateLimit, obj.StableKey, srv.ServiceVersionID, identity)
	if err != nil {
		return ratelimitpolicy.AcquireRequest{}, err
	}
	expiresAt, _ := ctx.Deadline()
	if hasActiveConcurrencyPolicy(policies) && expiresAt.IsZero() {
		return ratelimitpolicy.AcquireRequest{}, errors.New("provider concurrency requires a bounded execution deadline")
	}
	return ratelimitpolicy.AcquireRequest{AccountID: identity.AccountID, ServiceVersionID: srv.ServiceVersionID, PermitID: uuid.New(), PermitExpiresAt: expiresAt, Policies: policies}, nil
}

func validProviderRateLimitIdentity(identity providerRateLimitIdentity, present bool, serviceVersionID uuid.UUID) bool {
	return present && identity.AccountID != uuid.Nil && serviceVersionID != uuid.Nil
}

func resolveProviderRateLimitPolicies(config *ratelimitpolicy.Config, stableKey string, serviceVersionID uuid.UUID, identity providerRateLimitIdentity) ([]ratelimitpolicy.ResolvedPolicy, error) {
	policies := make([]ratelimitpolicy.ResolvedPolicy, 0, len(config.Policies))
	for _, policy := range config.Policies {
		resolved, err := resolveProviderRateLimitPolicyV3(policy, stableKey, serviceVersionID, identity)
		if err != nil {
			return nil, err
		}
		policies = append(policies, resolved)
	}
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Name != policies[j].Name {
			return policies[i].Name < policies[j].Name
		}
		return policies[i].ScopeID < policies[j].ScopeID
	})
	return policies, nil
}

func hasActiveConcurrencyPolicy(policies []ratelimitpolicy.ResolvedPolicy) bool {
	for _, policy := range policies {
		if policy.Algorithm == string(ratelimitpolicy.AlgorithmConcurrency) && policy.Cost > 0 {
			return true
		}
	}
	return false
}

func allRateLimitCostsZero(policies []ratelimitpolicy.ResolvedPolicy) bool {
	for _, policy := range policies {
		if policy.Cost > 0 {
			return false
		}
	}
	return true
}

func resolveProviderRateLimitPolicyV3(policy ratelimitpolicy.Policy, stableKey string, serviceVersionID uuid.UUID, identity providerRateLimitIdentity) (ratelimitpolicy.ResolvedPolicy, error) {
	scopeKind, scopeID, err := resolveQuotaIdentity(policy.Identity, serviceVersionID, identity)
	if err != nil {
		return ratelimitpolicy.ResolvedPolicy{}, err
	}
	resolved := ratelimitpolicy.ResolvedPolicy{Name: policy.Name, Mode: string(policy.Mode), Unit: string(policy.Unit), ScopeKind: scopeKind, ScopeID: scopeID.String(), Cost: resolvedV3Cost(policy.Cost, stableKey), Algorithm: string(policy.Algorithm)}
	applyResolvedAlgorithm(&resolved, policy)
	resolved.ConfigHash = providerRateLimitConfigHash(resolved)
	return resolved, nil
}

func resolvedV3Cost(plan ratelimitpolicy.CostPlan, stableKey string) int64 {
	for _, rule := range plan.Rules {
		if rule.Operation == stableKey {
			return rule.Cost
		}
	}
	return plan.Default
}

func resolveQuotaIdentity(config ratelimitpolicy.BucketIdentity, serviceVersionID uuid.UUID, identity providerRateLimitIdentity) (string, uuid.UUID, error) {
	kinds := make([]string, 0, len(config.Inputs))
	values := make([]string, 0, len(config.Inputs))
	for _, input := range config.Inputs {
		value, err := quotaIdentityValue(input, serviceVersionID, identity)
		if err != nil {
			return "", uuid.Nil, err
		}
		kinds = append(kinds, string(input.Kind))
		values = append(values, string(input.Kind)+"="+value)
	}
	// Only the deterministic digest crosses the coordinator boundary; raw
	// tenant, project, resource, and credential-family values stay local.
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	scopeID, err := uuid.FromBytes(sum[:16])
	if err != nil {
		return "", uuid.Nil, err
	}
	return strings.Join(kinds, "+"), scopeID, nil
}

func quotaIdentityValue(input ratelimitpolicy.IdentityInput, serviceVersionID uuid.UUID, identity providerRateLimitIdentity) (string, error) {
	switch input.Kind {
	case ratelimitpolicy.IdentityAccount:
		return identity.AccountID.String(), nil
	case ratelimitpolicy.IdentityServiceVersion:
		return serviceVersionID.String(), nil
	case ratelimitpolicy.IdentityConnection:
		return requiredUUID(identity.ConnectionID, "connection")
	case ratelimitpolicy.IdentityNamedSharedCredentialFamily:
		return input.Name, nil
	default:
		value := identity.Bindings[input.Binding]
		if value == "" {
			return "", errors.New("provider quota identity binding is unavailable")
		}
		return value, nil
	}
}

func requiredUUID(value uuid.UUID, kind string) (string, error) {
	if value == uuid.Nil {
		return "", errors.New("provider quota " + kind + " identity is unavailable")
	}
	return value.String(), nil
}

func applyResolvedAlgorithm(resolved *ratelimitpolicy.ResolvedPolicy, policy ratelimitpolicy.Policy) {
	if policy.FixedWindow != nil {
		resolved.Limit, resolved.DurationMs = policy.FixedWindow.Limit, policy.FixedWindow.DurationMs
	}
	if policy.RollingWindow != nil {
		resolved.RollingLimit, resolved.RollingDurationMs = policy.RollingWindow.Limit, policy.RollingWindow.DurationMs
	}
	if policy.TokenBucket != nil {
		resolved.Capacity, resolved.RefillUnits, resolved.RefillIntervalMs = policy.TokenBucket.Capacity, policy.TokenBucket.RefillUnits, policy.TokenBucket.RefillIntervalMs
	}
	if policy.Concurrency != nil {
		resolved.ConcurrencyLimit = policy.Concurrency.Limit
	}
}

func providerRateLimitConfigHash(policy ratelimitpolicy.ResolvedPolicy) string {
	config := struct {
		Algorithm         string `json:"algorithm"`
		Limit             int64  `json:"limit"`
		DurationMs        int64  `json:"duration_ms"`
		Capacity          int64  `json:"capacity"`
		RefillUnits       int64  `json:"refill_units"`
		RefillIntervalMs  int64  `json:"refill_interval_ms"`
		RollingLimit      int64  `json:"rolling_limit"`
		RollingDurationMs int64  `json:"rolling_duration_ms"`
		ConcurrencyLimit  int64  `json:"concurrency_limit"`
		Mode              string `json:"mode"`
	}{policy.Algorithm, policy.Limit, policy.DurationMs, policy.Capacity, policy.RefillUnits, policy.RefillIntervalMs, policy.RollingLimit, policy.RollingDurationMs, policy.ConcurrencyLimit, policy.Mode}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func attachRateLimitAggregates(decision ratelimitpolicy.Decision, policies []ratelimitpolicy.ResolvedPolicy) ratelimitpolicy.Decision {
	decision.PolicyCount = int64(len(policies))
	decision.UnitTotals = make(map[string]int64)
	scopes := make(map[string]struct{})
	for _, policy := range policies {
		decision.UnitTotals[policy.Unit] += policy.Cost
		for _, scope := range strings.Split(policy.ScopeKind, "+") {
			if validTelemetryScopeKind(scope) {
				scopes[scope] = struct{}{}
			}
		}
	}
	for scope := range scopes {
		decision.ScopeKinds = append(decision.ScopeKinds, scope)
	}
	sort.Strings(decision.ScopeKinds)
	return decision
}

func validTelemetryScopeKind(value string) bool {
	switch ratelimitpolicy.IdentityKind(value) {
	case ratelimitpolicy.IdentityAccount, ratelimitpolicy.IdentityServiceVersion, ratelimitpolicy.IdentityConnection, ratelimitpolicy.IdentityProject, ratelimitpolicy.IdentityTenant, ratelimitpolicy.IdentityResource, ratelimitpolicy.IdentityIPClass, ratelimitpolicy.IdentityNamedSharedCredentialFamily:
		return true
	default:
		return false
	}
}

func recordProviderRateLimitDecision(span trace.Span, decision ratelimitpolicy.Decision, err error) {
	outcome := "denied"
	if decision.Allowed {
		outcome = "allowed"
	}
	if decision.Allowed && decision.ObservedDenials > 0 {
		outcome = "would_deny"
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
		attribute.Int64("rate_limit.observed_denials", decision.ObservedDenials),
	)
	if err != nil {
		span.SetStatus(codes.Error, "acquisition_failed")
	}
}
