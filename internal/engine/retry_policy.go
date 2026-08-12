package engine

import (
	"context"
	"net/http"
	"strings"
)

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
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodTrace, "QUERY":
		return false
	default:
		// Extension methods have no standard idempotency guarantee. Requiring a
		// provider key is safer than guessing their replay semantics.
		return true
	}
}
