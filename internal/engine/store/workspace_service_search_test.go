package store

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestWorkspaceServiceSearch exercises real PostgreSQL matching, pagination, and authorization together.
func TestWorkspaceServiceSearch(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	allowed, other, denied := uuid.New(), uuid.New(), uuid.New()
	// Explicit dates make offset assertions independent of insertion timing.
	_, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_slug, service_name, created_at)
 VALUES ($1, '@payments/stripe-checkout', 'Stripe Checkout', '2026-01-03'),
 ($2, 'stripe-billing', 'Stripe Billing', '2026-01-02'),
 ($3, 'stripe-private', 'Stripe Private', '2026-01-01')`, allowed, other, denied)
	// Invalid fixtures would hide search regressions behind empty data.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version)
 VALUES ($1, gen_random_uuid(), '1.0.0'), ($2, gen_random_uuid(), '1.0.0'), ($3, gen_random_uuid(), '1.0.0')`, allowed, other, denied)
	// Page projection requires an enabled version for each test service.
	if err != nil {
		t.Fatal(err)
	}
	scope := accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowed, other}}
	for _, tc := range []struct {
		name, query   string
		offset, total int
		want          uuid.UUID
	}{
		{"partial case insensitive", " StRi ", 0, 2, allowed},
		{"second page", "stripe", 1, 2, other},
		{"provider slug", "@PAYMENTS/", 0, 1, allowed},
		{"service slug", "stripe-bill", 0, 1, other},
		{"no match", "missing", 0, 0, uuid.Nil},
		{"literal percent", "%", 0, 0, uuid.Nil},
		{"literal underscore", "_", 0, 0, uuid.Nil},
		{"unauthorized match", "private", 0, 0, uuid.Nil},
		{"cleared query", "", 0, 2, allowed},
	} {
		// Each case validates both the total and row, preventing a count-only authorization leak.
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := repository.SearchAuthorizedWorkspaceServicesPage(ctx, scope, nil, tc.query, 1, tc.offset)
			// Search errors and incorrect totals invalidate the visible pagination contract.
			if err != nil || total != tc.total {
				t.Fatalf("total=%d err=%v, want %d", total, err, tc.total)
			}
			// Empty queries and actual nonmatches have intentionally different result sets.
			if tc.want == uuid.Nil {
				if len(rows) != 0 {
					t.Fatalf("unexpected rows: %v", rows)
				}
			} else if len(rows) != 1 || rows[0].ServiceID != tc.want {
				t.Fatalf("unexpected page: %v", rows)
			}
		})
	}
	rows, total, err := repository.ListAuthorizedWorkspaceServicesPage(ctx, scope, []string{"stri"}, 10, 0)
	// Legacy names remain exact identifiers rather than silently acquiring fuzzy semantics.
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("exact filter changed: %v/%d/%v", rows, total, err)
	}
	rows, total, err = repository.SearchAuthorizedWorkspaceServicesPage(ctx, accesscontrol.AuthorizedScope{}, nil, "stripe", 10, 0)
	// A user without service grants cannot learn matching names or counts.
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("empty scope leaked rows: %v/%d/%v", rows, total, err)
	}
}
