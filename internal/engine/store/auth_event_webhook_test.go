package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mustExecAuthEventFixture persists one relational fixture row or stops the PostgreSQL integration test.
func mustExecAuthEventFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, arguments ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, query, arguments...)
	// Fixture failure cannot be interpreted as a resolver behavior failure.
	if err != nil {
		t.Fatalf("seed auth event resolver fixture: %v", err)
	}
}

// TestResolveAuthEventAppFamiliesPreservesTombstonedFamilyContinuity proves old-version provenance reaches a live sibling's stable family.
func TestResolveAuthEventAppFamiliesPreservesTombstonedFamilyContinuity(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// PostgreSQL verification is opt-in so ordinary unit runs never mutate a developer database accidentally.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	t.Cleanup(pool.Close)
	repository := NewPostgresStore(pool).(*postgresStore)
	accountID := uuid.New()
	familyID := uuid.New()
	activeAppID := uuid.New()
	tombstonedAppID := uuid.New()
	teamID := seedAppOwnerTeam(t, ctx, pool)

	mustExecAuthEventFixture(t, ctx, pool, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'sdk', $3, 'Auth events SDK', 'typescript', $4)
	`, familyID, accountID, familyID.String(), teamID)
	mustExecAuthEventFixture(t, ctx, pool, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key, source_hash, status)
		VALUES ($1, $2, $3, '2.0.0', $4, 'sha256:active', 'active')
	`, activeAppID, familyID, accountID, "sdk:auth-events:"+activeAppID.String())
	mustExecAuthEventFixture(t, ctx, pool, `
		INSERT INTO fused_app_tombstones
			(app_id, app_family_id, account_id, version, source_hash)
		VALUES ($1, $2, $3, '1.0.0', 'sha256:tombstoned')
	`, tombstonedAppID, familyID, accountID)

	resolved, err := repository.ResolveAuthEventAppFamilies(ctx, []uuid.UUID{activeAppID, tombstonedAppID, uuid.New()})
	// Resolver failures would make the test unable to distinguish missing provenance from database uncertainty.
	if err != nil {
		t.Fatalf("ResolveAuthEventAppFamilies: %v", err)
	}
	// Unknown app identity stays absent while both admitted provenance rows resolve.
	if len(resolved) != 2 {
		t.Fatalf("resolved identities = %#v", resolved)
	}
	// Active and tombstoned provenance must resolve to the same stable SDK family identity.
	for _, appID := range []uuid.UUID{activeAppID, tombstonedAppID} {
		identity := resolved[appID]
		// Both immutable versions must resolve to the same family/account/kind used by the projected webhook subject.
		if identity.AppFamilyID != familyID || identity.AccountID != accountID || identity.Kind != AppKindSDK {
			t.Fatalf("identity for %s = %#v", appID, identity)
		}
	}
}
