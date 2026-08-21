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
		name   string
		intent *PaginationIntent
		policy *models.PaginationConfig
		valid  bool
	}{
		{name: "omitted", intent: nil, policy: nil, valid: true},
		{name: "strict reduction", intent: &PaginationIntent{MaxPages: 1}, policy: policy, valid: true},
		{name: "zero", intent: &PaginationIntent{MaxPages: 0}, policy: policy},
		{name: "ceiling exceeded", intent: &PaginationIntent{MaxPages: paginationpolicy.CeilingMaxPages + 1}, policy: policy},
		{name: "non paginated", intent: &PaginationIntent{MaxPages: 1}, policy: nil},
		{name: "equal policy", intent: &PaginationIntent{MaxPages: 10}, policy: policy},
		{name: "above policy", intent: &PaginationIntent{MaxPages: 11}, policy: policy},
	}
	// Every invalid shape must resolve to the same secret-safe public sentinel.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePaginationIntentPolicy(test.intent, test.policy)
			if test.valid && err != nil {
				t.Fatalf("ValidatePaginationIntentPolicy() error = %v", err)
			}
			if !test.valid && !errors.Is(err, ErrPaginationIntentInvalid) {
				t.Fatalf("ValidatePaginationIntentPolicy() error = %v", err)
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
