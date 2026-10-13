package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestUnifiedAppNameLookupUsesTokenFamily verifies tenant isolation and live-token restrictions against PostgreSQL.
func TestUnifiedAppNameLookupUsesTokenFamily(t *testing.T) {
	first, second := newAppTokenPolicyFixture(t), newAppTokenPolicyFixture(t)
	name := "shared-name-" + uuid.NewString()
	for _, fixture := range []appTokenPolicyFixture{first, second} {
		fixture.createToken(t, fixture.familyID.String(), AppTokenPolicy{AllowAll: true})
		// MCP families cannot opt into Unified App name routing even with a valid token.
		assertUnifiedAppNameNotFound(t, fixture, "token-policy-"+fixture.familyID.String(), fixture.familyID.String()+"-hash")
		// Two workspaces deliberately share one canonical name to prove the token chooses the tenant.
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE fused_app_families SET kind='unified_app',target_language='typescript',canonical_name=$1 WHERE app_family_id=$2`, name, fixture.familyID); err != nil {
			t.Fatal(err)
		}
		id, err := fixture.repository.ResolveUnifiedAppName(fixture.ctx, name, fixture.familyID.String()+"-hash")
		// The shared spelling must still resolve to the token's own family.
		if err != nil || id != fixture.familyID {
			t.Fatalf("resolved=%s want=%s err=%v", id, fixture.familyID, err)
		}
	}
	expired := time.Now().Add(-time.Hour)
	first.createToken(t, "expired-name-"+first.familyID.String(), AppTokenPolicy{AllowAll: true, ExpiresAt: &expired})
	for _, test := range []struct{ name, hash string }{
		{"wrong-name", first.familyID.String() + "-hash"}, {name, "foreign-hash"}, {name, "expired-name-" + first.familyID.String() + "-hash"},
	} {
		// Unknown names and unusable tokens must remain indistinguishable.
		assertUnifiedAppNameNotFound(t, first, test.name, test.hash)
	}
	// Revocation removes the token; the uncached resolver must stop accepting it immediately.
	if _, err := first.pool.Exec(first.ctx, `DELETE FROM fused_app_tokens WHERE app_family_id=$1`, first.familyID); err != nil {
		t.Fatal(err)
	}
	assertUnifiedAppNameNotFound(t, first, name, first.familyID.String()+"-hash")
	// An archived family must not be routable even when it retains a live historical token.
	if _, err := second.pool.Exec(second.ctx, `UPDATE fused_app_families SET archived_at=NOW() WHERE app_family_id=$1`, second.familyID); err != nil {
		t.Fatal(err)
	}
	assertUnifiedAppNameNotFound(t, second, name, second.familyID.String()+"-hash")
}

// assertUnifiedAppNameNotFound verifies lookup denials do not disclose a family's identity.
func assertUnifiedAppNameNotFound(t *testing.T, fixture appTokenPolicyFixture, name, hash string) {
	t.Helper()
	_, err := fixture.repository.ResolveUnifiedAppName(fixture.ctx, name, hash)
	// All unusable references share one bounded lookup outcome.
	if !errors.Is(err, ErrAppFamilyNotFound) {
		t.Fatalf("denial error=%v", err)
	}
}
