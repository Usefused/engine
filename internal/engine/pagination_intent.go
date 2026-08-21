package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

// ErrPaginationIntentInvalid identifies caller pagination controls that do not strictly tighten a reviewed policy.
var ErrPaginationIntentInvalid = errors.New("pagination intent is invalid")

// PaginationIntent is the provider-neutral request control retained inside one physical execution.
type PaginationIntent struct {
	MaxPages int
}

type paginationIntentContextKey struct{}

// ValidatePaginationIntent rejects absent bounds and values outside the shared hard policy ceiling.
func ValidatePaginationIntent(intent *PaginationIntent) error {
	// An omitted intent preserves automatic pagination and requires no validation.
	if intent == nil {
		return nil
	}
	// A present intent must name a useful positive bound that cannot exceed the global safety ceiling.
	if intent.MaxPages < 1 || intent.MaxPages > paginationpolicy.CeilingMaxPages {
		return ErrPaginationIntentInvalid
	}
	return nil
}

// ValidatePaginationIntentPolicy permits only controls that strictly reduce a paginated operation's effective page limit.
func ValidatePaginationIntentPolicy(intent *PaginationIntent, policy *models.PaginationConfig) error {
	// Requests without pagination intent retain the immutable operation behavior.
	if intent == nil {
		return nil
	}
	if err := ValidatePaginationIntent(intent); err != nil {
		return err
	}
	// Non-paginated operations cannot silently accept a decorative pagination control.
	if policy == nil {
		return ErrPaginationIntentInvalid
	}
	limits := paginationpolicy.EffectiveLimits((*paginationpolicy.Config)(policy).Limits)
	// Equality would soften the policy's hard max-pages failure into partial success, so only a strict reduction is allowed.
	if intent.MaxPages >= limits.MaxPages {
		return ErrPaginationIntentInvalid
	}
	return nil
}

// ContextWithPaginationIntent attaches validated request intent without mutating provider parameters or immutable policy.
func ContextWithPaginationIntent(ctx context.Context, intent *PaginationIntent) context.Context {
	// Nil intent leaves the request context unchanged and allocation-free.
	if intent == nil {
		return ctx
	}
	copyValue := *intent
	return context.WithValue(ctx, paginationIntentContextKey{}, copyValue)
}

// PaginationIntentFromContext returns a copy of the bounded request intent when one was supplied.
func PaginationIntentFromContext(ctx context.Context) (PaginationIntent, bool) {
	// A nil context cannot carry request-scoped execution controls.
	if ctx == nil {
		return PaginationIntent{}, false
	}
	intent, ok := ctx.Value(paginationIntentContextKey{}).(PaginationIntent)
	return intent, ok
}

// BindPaginationIntentRequestHash includes result-size intent in replay identity while preserving legacy hashes when omitted.
func BindPaginationIntentRequestHash(requestHash string, intent *PaginationIntent) string {
	// Existing callers without intent retain byte-identical idempotency behavior.
	if intent == nil {
		return requestHash
	}
	digest := sha256.Sum256([]byte(requestHash + "\x00pagination.max_pages=" + strconv.Itoa(intent.MaxPages)))
	return hex.EncodeToString(digest[:])
}
