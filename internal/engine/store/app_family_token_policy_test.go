package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAssessAppCapabilityExpansionCountsOnlyAffectedActiveTokens(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	prefix := "service:" + uuid.NewString() + ":" + uuid.NewString()
	existingOperation := prefix + ":operation:list"
	fixture.addCapability(t, existingOperation)

	fixture.createToken(t, "all", AppTokenPolicy{AllowAll: true})
	fixture.createToken(t, "create", AppTokenPolicy{AllowedOperations: []string{"create"}})
	fixture.createToken(t, "delete", AppTokenPolicy{AllowedOperations: []string{"delete"}})
	specialOperation := "create:special\nv2"
	fixture.createToken(t, "special", AppTokenPolicy{AllowedOperations: []string{specialOperation}})
	expired := time.Now().Add(-time.Minute)
	fixture.createToken(t, "expired-create", AppTokenPolicy{
		AllowedOperations: []string{"create"},
		ExpiresAt:         &expired,
	})

	expands, affected, err := fixture.repository.AssessAppCapabilityExpansion(
		fixture.ctx,
		fixture.familyID,
		[]string{existingOperation, prefix + ":operation:create"},
	)
	if err != nil {
		t.Fatalf("assess operation expansion: %v", err)
	}
	if !expands || affected != 2 {
		t.Fatalf("operation expansion = (%v, %d), want (true, 2)", expands, affected)
	}

	expands, affected, err = fixture.repository.AssessAppCapabilityExpansion(
		fixture.ctx,
		fixture.familyID,
		[]string{existingOperation, prefix + ":operation:" + specialOperation},
	)
	if err != nil {
		t.Fatalf("assess delimited operation expansion: %v", err)
	}
	if !expands || affected != 2 {
		t.Fatalf("delimited operation expansion = (%v, %d), want (true, 2)", expands, affected)
	}

	// Non-operation capability changes still affect wildcard tokens, but do not
	// broaden a strict token's explicit operation set.
	expands, affected, err = fixture.repository.AssessAppCapabilityExpansion(
		fixture.ctx,
		fixture.familyID,
		[]string{existingOperation, prefix + ":connect-scope:write"},
	)
	if err != nil {
		t.Fatalf("assess non-operation expansion: %v", err)
	}
	if !expands || affected != 1 {
		t.Fatalf("non-operation expansion = (%v, %d), want (true, 1)", expands, affected)
	}

	expands, affected, err = fixture.repository.AssessAppCapabilityExpansion(
		fixture.ctx,
		fixture.familyID,
		[]string{existingOperation},
	)
	if err != nil {
		t.Fatalf("assess unchanged capability set: %v", err)
	}
	if expands || affected != 0 {
		t.Fatalf("unchanged capability set = (%v, %d), want (false, 0)", expands, affected)
	}

	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM fused_apps WHERE app_id = $1`, fixture.appID); err != nil {
		t.Fatalf("remove runnable app: %v", err)
	}
	expands, affected, err = fixture.repository.AssessAppCapabilityExpansion(
		fixture.ctx,
		fixture.familyID,
		[]string{prefix + ":operation:create"},
	)
	if err != nil {
		t.Fatalf("assess family without runnable apps: %v", err)
	}
	if expands || affected != 0 {
		t.Fatalf("family without runnable apps = (%v, %d), want (false, 0)", expands, affected)
	}
}

func TestAppTokenPolicyPersistenceAndExpiryAuthorization(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	created := fixture.createToken(t, "strict", AppTokenPolicy{
		AllowedOperations: []string{"create", "list"},
		ExpiresAt:         &future,
	})
	if created.AllowAll || len(created.AllowedOperations) != 2 || created.ExpiresAt == nil {
		t.Fatalf("unexpected created token metadata: %#v", created)
	}

	projection, err := fixture.repository.AuthorizeApp(fixture.ctx, fixture.appID, "strict-hash")
	if err != nil {
		t.Fatalf("authorize strict token: %v", err)
	}
	if projection.TokenPolicy.AllowAll || len(projection.TokenPolicy.AllowedOperations) != 2 {
		t.Fatalf("unexpected authorization policy: %#v", projection.TokenPolicy)
	}
	if projection.TokenID != created.ID {
		t.Fatalf("authorization token id = %s, want %s", projection.TokenID, created.ID)
	}

	expired := time.Now().Add(-time.Minute)
	fixture.createToken(t, "expired", AppTokenPolicy{AllowAll: true, ExpiresAt: &expired})
	_, err = fixture.repository.AuthorizeApp(fixture.ctx, fixture.appID, "expired-hash")
	if !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("expired token authorization error = %v, want %v", err, ErrAppNotFound)
	}

	tokens, err := fixture.repository.ListAppTokens(fixture.ctx, fixture.familyID)
	if err != nil {
		t.Fatalf("list app tokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("listed %d tokens, want 2", len(tokens))
	}

	revocation, err := fixture.repository.RevokeAppToken(fixture.ctx, fixture.familyID, "strict")
	if err != nil {
		t.Fatalf("revoke strict token: %v", err)
	}
	if revocation.TokenID != created.ID || revocation.AppFamilyID != fixture.familyID || revocation.RevokedAt.IsZero() {
		t.Fatalf("revocation projection = %#v", revocation)
	}
	if _, err := fixture.repository.AuthorizeApp(fixture.ctx, fixture.appID, "strict-hash"); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("revoked token authorization error = %v, want %v", err, ErrAppNotFound)
	}
}

type appTokenPolicyFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	repository *postgresStore
	familyID   uuid.UUID
	appID      uuid.UUID
}

func newAppTokenPolicyFixture(t *testing.T) appTokenPolicyFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL is required for app token policy integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		cancel()
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(cancel)
	t.Cleanup(pool.Close)

	familyID, appID, accountID := uuid.New(), uuid.New(), uuid.New()
	ownerTeamID := seedAppOwnerTeam(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, owner_team_id)
		VALUES ($1, $2, 'mcp', $3, 'Token policy test', $4)
	`, familyID, accountID, "token-policy-"+familyID.String(), ownerTeamID); err != nil {
		t.Fatalf("seed app family: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key, source_hash, status)
		VALUES ($1, $2, $3, '1.0.0', $4, 'token-policy-test', 'active')
	`, appID, familyID, accountID, "mcp:token-policy:"+familyID.String()); err != nil {
		t.Fatalf("seed app version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_teams WHERE id = $1`, ownerTeamID)
	})

	return appTokenPolicyFixture{
		ctx:        ctx,
		pool:       pool,
		repository: NewPostgresStore(pool).(*postgresStore),
		familyID:   familyID,
		appID:      appID,
	}
}

func (fixture appTokenPolicyFixture) addCapability(t *testing.T, capabilityKey string) {
	t.Helper()
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_app_capabilities (app_id, capability_key) VALUES ($1, $2)
	`, fixture.appID, capabilityKey); err != nil {
		t.Fatalf("seed app capability: %v", err)
	}
}

func (fixture appTokenPolicyFixture) createToken(t *testing.T, name string, policy AppTokenPolicy) *AppTokenMetadata {
	t.Helper()
	token, err := fixture.repository.CreateAppToken(
		fixture.ctx,
		fixture.familyID,
		name+"-hash",
		name,
		policy,
	)
	if err != nil {
		t.Fatalf("create %q app token: %v", name, err)
	}
	return token
}
