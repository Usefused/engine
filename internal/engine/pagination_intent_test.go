package engine

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

// TestValidatePaginationIntentPolicyRequiresStrictReduction pins the caller-versus-policy security boundary.
func TestValidatePaginationIntentPolicyRequiresStrictReduction(t *testing.T) {
	policy := paginationIntentPolicy(10)
	tests := []struct {
		name       string
		intent     *PaginationIntent
		policy     *models.PaginationConfig
		valid      bool
		wantReason PaginationIntentErrorReason
		wantLimit  int
	}{
		{name: "omitted", intent: nil, policy: nil, valid: true},
		{name: "strict reduction", intent: &PaginationIntent{MaxPages: 1}, policy: policy, valid: true},
		{name: "zero", intent: &PaginationIntent{MaxPages: 0}, policy: policy, wantReason: PaginationIntentInvalidValue},
		{name: "ceiling exceeded", intent: &PaginationIntent{MaxPages: paginationpolicy.CeilingMaxPages + 1}, policy: policy, wantReason: PaginationIntentInvalidValue},
		{name: "non paginated", intent: &PaginationIntent{MaxPages: 1}, policy: nil, wantReason: PaginationIntentNotSupported},
		{name: "equal policy", intent: &PaginationIntent{MaxPages: 10}, policy: policy, wantReason: PaginationIntentBoundNotLower, wantLimit: 10},
		{name: "above policy", intent: &PaginationIntent{MaxPages: 11}, policy: policy, wantReason: PaginationIntentBoundNotLower, wantLimit: 10},
	}
	// Every invalid shape retains sentinel compatibility while preserving its safe correction reason.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePaginationIntentPolicy(test.intent, test.policy)
			// Valid controls must remain free of a typed validation failure.
			if test.valid && err != nil {
				t.Fatalf("ValidatePaginationIntentPolicy() error = %v", err)
			}
			// Existing callers continue to classify every rejected control through the stable sentinel.
			if !test.valid && !errors.Is(err, ErrPaginationIntentInvalid) {
				t.Fatalf("ValidatePaginationIntentPolicy() error = %v", err)
			}
			// Valid cases have no error details to inspect.
			if test.valid {
				return
			}
			var validationErr *PaginationIntentValidationError
			// Public adapters need the precise reason and effective limit without re-resolving policy.
			if !errors.As(err, &validationErr) || validationErr.Reason != test.wantReason || validationErr.EngineMaxPages != test.wantLimit {
				t.Fatalf("validation error = %#v, want reason=%q limit=%d", validationErr, test.wantReason, test.wantLimit)
			}
		})
	}
}

// TestBindPaginationIntentRequestHashChangesOnlyBoundedIntent proves replay identity includes caller truncation.
func TestBindPaginationIntentRequestHashChangesOnlyBoundedIntent(t *testing.T) {
	base := "body-hash"
	if got := BindPaginationIntentRequestHash(base, nil); got != base {
		t.Fatalf("omitted intent hash = %q", got)
	}
	one := BindPaginationIntentRequestHash(base, &PaginationIntent{MaxPages: 1})
	two := BindPaginationIntentRequestHash(base, &PaginationIntent{MaxPages: 2})
	if one == base || one == two || len(one) != 64 || len(two) != 64 {
		t.Fatalf("bound hashes = %q / %q", one, two)
	}
}

// paginationIntentPolicy returns one reviewed v3 policy with the requested page safety limit.
func paginationIntentPolicy(maxPages int) *models.PaginationConfig {
	policy := paginationpolicy.Config{Limits: paginationpolicy.Limits{
		MaxPages: maxPages, MaxItems: 100, MaxBytes: 1 << 20, MaxDurationMs: 5_000,
	}}
	mapped := models.PaginationConfig(policy)
	return &mapped
}
