package engine

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/shared/models"
)

// sdkRetryCeilingMaxRetries and sdkRetryCeilingBackoffMs bound an SDK-supplied
// RetryOverride when the service has no RetryConfig of its own to bound
// against. Without this ceiling a buggy or malicious caller could force
// Engine into an unbounded retry storm against a provider.
const (
	sdkRetryCeilingMaxRetries = 5
	sdkRetryCeilingBackoffMs  = 10_000
	defaultRetryStrategy      = "fixed"
)

// RetryOverride is a caller-supplied (SDK-side) request to bound retry
// behavior for a single Execute call. MaxRetries and BackoffMs are pointers
// so "not set" (nil) is distinguishable from "explicitly 0". Strategy is
// intentionally not part of this type: the backoff algorithm is always owned
// by the service's RetryConfig (or defaultRetryStrategy when the service has
// none), never by the caller -- see resolveRetryPolicy.
type RetryOverride struct {
	MaxRetries *int
	BackoffMs  *int
}

type retryOverrideContextKey struct{}

// ContextWithRetryOverride attaches an SDK-supplied retry override to ctx for
// the dispatcher to read inside executeWithRetries. A nil override is a
// no-op so callers can pass through unconditionally.
func ContextWithRetryOverride(ctx context.Context, override *RetryOverride) context.Context {
	if override == nil {
		return ctx
	}
	return context.WithValue(ctx, retryOverrideContextKey{}, override)
}

// RetryOverrideFromContext returns the override attached by
// ContextWithRetryOverride, or nil if none was set.
func RetryOverrideFromContext(ctx context.Context) *RetryOverride {
	v, _ := ctx.Value(retryOverrideContextKey{}).(*RetryOverride)
	return v
}

// resolveRetryPolicy decides the retry behavior for one dispatch call. Engine
// never retries unless something explicitly says it may -- there is no
// hardcoded fallback:
//
//   - No service config, no override -> nil: single attempt, no retry.
//   - No service config, override present -> the override's MaxRetries/
//     BackoffMs, clamped to the hard SDK ceiling above (there's no service
//     policy to bound against instead). Strategy defaults to "fixed".
//   - Service config present, no override -> the service config, unchanged.
//   - Both present -> the override can only narrow the service's ceiling,
//     never widen it: MaxRetries/BackoffMs are clamped to the service's own
//     values. Strategy always comes from the service config -- the caller
//     cannot request a different backoff algorithm than the service defines.
func resolveRetryPolicy(service *models.RetryConfig, override *RetryOverride) *models.RetryConfig {
	if service == nil {
		if override == nil {
			return nil
		}
		return &models.RetryConfig{
			Strategy:   defaultRetryStrategy,
			MaxRetries: clampInt(intOrZero(override.MaxRetries), 0, sdkRetryCeilingMaxRetries),
			BackoffMs:  clampInt(intOrZero(override.BackoffMs), 0, sdkRetryCeilingBackoffMs),
		}
	}
	if override == nil {
		return service
	}
	resolved := &models.RetryConfig{
		Strategy:   service.Strategy,
		MaxRetries: service.MaxRetries,
		BackoffMs:  service.BackoffMs,
	}
	if override.MaxRetries != nil {
		resolved.MaxRetries = clampInt(*override.MaxRetries, 0, service.MaxRetries)
	}
	if override.BackoffMs != nil {
		resolved.BackoffMs = clampInt(*override.BackoffMs, 0, service.BackoffMs)
	}
	return resolved
}

type idempotencyKeyPresentContextKey struct{}

// ContextWithIdempotencyKeyPresent records whether the caller supplied an
// idempotency key for this call. Only true is ever stored -- ctx simply has
// no value for this key when false, which IdempotencyKeyPresentInContext
// already treats as "not present".
func ContextWithIdempotencyKeyPresent(ctx context.Context, present bool) context.Context {
	if !present {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyPresentContextKey{}, true)
}

// IdempotencyKeyPresentInContext reports whether the caller supplied an
// idempotency key for this call. Used by methodSafeToRetryWithoutKey to
// decide whether a non-idempotent method may be retried on a 5xx.
func IdempotencyKeyPresentInContext(ctx context.Context) bool {
	v, _ := ctx.Value(idempotencyKeyPresentContextKey{}).(bool)
	return v
}

// methodRequiresIdempotencyKeyForRetry reports whether method is not
// guaranteed idempotent by HTTP semantics -- a retried POST can create a
// second resource, a retried PATCH can double-apply a change. Retrying one
// of these on a 5xx is only safe if an idempotency key lets the vendor
// dedupe. GET/HEAD/PUT/DELETE/OPTIONS are idempotent by definition
// (repeating them has the same end effect) and are always safe to retry.
func methodRequiresIdempotencyKeyForRetry(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch:
		return true
	default:
		return false
	}
}

// resolveEffectiveMaxRetries applies the HTTP-method safety gate on top of an
// already-resolved policy: split out of executeWithRetries (separation of
// concerns, and keeps that function's branching within the repo's complexity
// budget) since "what retry count is this call allowed" is a self-contained
// decision distinct from "run the attempt loop". Every outcome is recorded
// on span -- retry is a per-call, user/agent-triggered decision, so it needs
// to be traceable for debugging/audit like the rest of the dispatch path.
func resolveEffectiveMaxRetries(ctx context.Context, span trace.Span, policy *models.RetryConfig, method string) int {
	maxRetries := 0
	if policy != nil {
		maxRetries = policy.MaxRetries
	}
	// POST/PATCH are not guaranteed idempotent by HTTP semantics, so retrying
	// one on a 5xx is only safe if the caller supplied an idempotency key the
	// vendor can dedupe on. GET/HEAD/PUT/DELETE are idempotent by definition
	// and are never gated here.
	if maxRetries > 0 && methodRequiresIdempotencyKeyForRetry(method) && !IdempotencyKeyPresentInContext(ctx) {
		maxRetries = 0
		span.SetAttributes(attribute.Bool("retry_disabled_no_idempotency_key", true))
	}
	span.SetAttributes(attribute.Int("retry_max_configured", maxRetries))
	return maxRetries
}

func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
