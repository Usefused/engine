package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

	fixture.assertExpansion(t, []string{existingOperation, prefix + ":operation:create"}, true, 2, "operation expansion")
	fixture.assertExpansion(t, []string{existingOperation, prefix + ":operation:" + specialOperation}, true, 2, "delimited operation expansion")

	// Non-operation capability changes still affect wildcard tokens, but do not
	// broaden a strict token's explicit operation set.
	fixture.assertExpansion(t, []string{existingOperation, prefix + ":connect-scope:write"}, true, 1, "non-operation expansion")
	fixture.assertExpansion(t, []string{existingOperation}, false, 0, "unchanged capability set")

	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM fused_apps WHERE app_id = $1`, fixture.appID); err != nil {
		t.Fatalf("remove runnable app: %v", err)
	}
	fixture.assertExpansion(t, []string{prefix + ":operation:create"}, false, 0, "family without runnable apps")
}

func TestAppTokenPolicyPersistenceAndExpiryAuthorization(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	created := fixture.createToken(t, "strict", AppTokenPolicy{
		AllowedOperations: []string{"create", "list"},
		ExpiresAt:         &future,
	})
	assertStrictTokenMetadata(t, created)
	fixture.assertStrictTokenAuthorization(t, created.ID)

	expired := time.Now().Add(-time.Minute)
	expiredToken := fixture.createToken(t, "expired", AppTokenPolicy{AllowAll: true, ExpiresAt: &expired})
	fixture.seedTokenUsage(t, expiredToken.ID)
	fixture.expireAndAssertRetained(t, expiredToken.ID)
	fixture.assertAuthorizationDenied(t, "expired-hash", "expired")
	fixture.assertListedTokenCount(t, 2)
	fixture.assertTokenUsage(t, expiredToken.ID, 1, 1)
	fixture.revokeAndAssertDenied(t, created.ID, "strict", "strict-hash")
}

func TestAppTokenFixedBindingsResolveAtomically(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	target := fixture.seedFixedBindingTarget(t)
	defer fixture.removeFixedBindingTarget(t, target)

	created := fixture.createFixedToken(t, "fixed", target.validRequests())
	fixture.assertResolvedBinding(t, created.ID, target.serviceA, "gmail", target.connectionA, nil)
	fixture.assertResolvedBinding(t, created.ID, target.serviceB, "drive", target.connectionB, &target.resourceB)

	invalidID := uuid.New()
	invalidRequests := target.validRequests()
	// Leaving one request valid proves issuance cannot retain a partially
	// resolved credential when another binding in the same batch is invalid.
	invalidRequests[1].EndUserRef = "missing-user"
	_, err := fixture.repository.CreateAppToken(fixture.ctx, fixedTokenIssue(invalidID, fixture.familyID, "invalid", invalidRequests))
	if !errors.Is(err, ErrAppTokenBindingInvalid) {
		t.Fatalf("invalid fixed binding error = %v, want %v", err, ErrAppTokenBindingInvalid)
	}
	fixture.assertTokenTransactionRolledBack(t, invalidID)

	aliasID := uuid.New()
	aliasRequests := []AppTokenBindingRequest{
		{ServiceSlug: target.serviceSlugA, AuthName: "gmail", EndUserRef: "mail-user"},
		{ServiceSlug: "@google/" + target.serviceSlugA, AuthName: "gmail", EndUserRef: "mail-user-b"},
	}
	_, err = fixture.repository.CreateAppToken(fixture.ctx, fixedTokenIssue(aliasID, fixture.familyID, "duplicate-alias", aliasRequests))
	if !errors.Is(err, ErrAppTokenBindingInvalid) {
		t.Fatalf("duplicate resolved service/auth error = %v, want %v", err, ErrAppTokenBindingInvalid)
	}
	fixture.assertTokenTransactionRolledBack(t, aliasID)
}

type fixedBindingTarget struct {
	bucketID     uuid.UUID
	serviceA     uuid.UUID
	serviceB     uuid.UUID
	serviceSlugA string
	serviceSlugB string
	connectionA  uuid.UUID
	connectionA2 uuid.UUID
	connectionB  uuid.UUID
	resourceB    uuid.UUID
}

func (target fixedBindingTarget) validRequests() []AppTokenBindingRequest {
	return []AppTokenBindingRequest{
		{ServiceSlug: "@google/" + target.serviceSlugA, AuthName: "gmail", EndUserRef: "mail-user"},
		{ServiceSlug: target.serviceSlugB, AuthName: "drive", EndUserRef: "drive-user", ResourceID: &target.resourceB},
	}
}

func fixedTokenIssue(id, familyID uuid.UUID, name string, bindings []AppTokenBindingRequest) AppTokenIssue {
	return AppTokenIssue{
		ID: id, AppFamilyID: familyID, TokenHash: name + "-hash", Name: name,
		Policy: AppTokenPolicy{AllowAll: true}, BindingMode: AppTokenBindingFixed, Bindings: bindings,
	}
}

func (fixture appTokenPolicyFixture) createFixedToken(t *testing.T, name string, bindings []AppTokenBindingRequest) *AppTokenMetadata {
	t.Helper()
	token, err := fixture.repository.CreateAppToken(fixture.ctx, fixedTokenIssue(uuid.New(), fixture.familyID, name, bindings))
	if err != nil {
		t.Fatalf("create fixed token: %v", err)
	}
	if token.BindingMode != AppTokenBindingFixed {
		t.Fatalf("binding mode = %q, want fixed", token.BindingMode)
	}
	return token
}

func (fixture appTokenPolicyFixture) seedFixedBindingTarget(t *testing.T) fixedBindingTarget {
	t.Helper()
	target := fixedBindingTarget{
		bucketID: uuid.New(), serviceA: uuid.New(), serviceB: uuid.New(),
		serviceSlugA: "gmail-fixed-" + uuid.NewString(), serviceSlugB: "google-drive-fixed-" + uuid.NewString(),
		connectionA: uuid.New(), connectionA2: uuid.New(), connectionB: uuid.New(), resourceB: uuid.New(),
	}
	if err := fixture.insertFixedBindingTarget(target); err != nil {
		t.Fatalf("seed fixed binding target: %v", err)
	}
	return target
}

func (fixture appTokenPolicyFixture) insertFixedBindingTarget(target fixedBindingTarget) error {
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(fixture.ctx)
	// A batch keeps the relational fixture atomic without relying on pgx's
	// unsupported multi-command prepared statements or adding network N+1 work.
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, target.bucketID, "fixed-binding-"+target.bucketID.String())
	batch.Queue(`INSERT INTO fused_app_family_buckets (app_family_id, bucket_id) VALUES ($1, $2)`, fixture.familyID, target.bucketID)
	batch.Queue(`INSERT INTO fused_workspace_services (service_id, service_slug, service_name)
		VALUES ($1, $2, 'Gmail fixed binding'), ($3, $4, $2)`,
		target.serviceA, target.serviceSlugA, target.serviceB, target.serviceSlugB)
	batch.Queue(`INSERT INTO fused_auth_connections
		(id, bucket_id, service_id, end_user_ref, auth_type, auth_name, encrypted_dek, access_token)
		VALUES ($1, $2, $3, 'mail-user', 'oauth2', 'gmail', 'encrypted', 'encrypted'),
		       ($4, $2, $3, 'mail-user-b', 'oauth2', 'gmail', 'encrypted', 'encrypted'),
		       ($5, $2, $6, 'drive-user', 'oauth2', 'drive', 'encrypted', 'encrypted')`,
		target.connectionA, target.bucketID, target.serviceA, target.connectionA2, target.connectionB, target.serviceB)
	batch.Queue(`INSERT INTO fused_connection_resources
		(id, connection_id, bucket_id, service_id, provider_resource_id, resource_type, display_name, base_url)
		VALUES ($1, $2, $3, $4, 'drive-root', 'drive', 'Drive root', 'https://www.googleapis.com')`,
		target.resourceB, target.connectionB, target.bucketID, target.serviceB)
	if err := executeFixedBindingBatch(tx.SendBatch(fixture.ctx, batch), batch.Len()); err != nil {
		return err
	}
	return tx.Commit(fixture.ctx)
}

func executeFixedBindingBatch(results pgx.BatchResults, statementCount int) error {
	for index := 0; index < statementCount; index++ {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return err
		}
	}
	return results.Close()
}

func (fixture appTokenPolicyFixture) removeFixedBindingTarget(t *testing.T, target fixedBindingTarget) {
	t.Helper()
	if err := fixture.deleteFixedBindingTarget(target); err != nil {
		t.Errorf("remove fixed binding target: %v", err)
	}
}

func (fixture appTokenPolicyFixture) deleteFixedBindingTarget(target fixedBindingTarget) error {
	ctx := context.Background()
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Cleanup uses the same single-round-trip transaction shape as setup so a
	// failed assertion cannot leave half-removed rows in the retained test DB.
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM fused_app_family_buckets WHERE app_family_id = $1 AND bucket_id = $2`, fixture.familyID, target.bucketID)
	batch.Queue(`DELETE FROM fused_buckets WHERE id = $1`, target.bucketID)
	batch.Queue(`DELETE FROM fused_workspace_services WHERE service_id = ANY($1::uuid[])`, []uuid.UUID{target.serviceA, target.serviceB})
	if err := executeFixedBindingBatch(tx.SendBatch(ctx, batch), batch.Len()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (fixture appTokenPolicyFixture) assertResolvedBinding(
	t *testing.T,
	tokenID, serviceID uuid.UUID,
	authName string,
	connectionID uuid.UUID,
	resourceID *uuid.UUID,
) {
	t.Helper()
	binding, err := fixture.repository.GetAppTokenBinding(fixture.ctx, tokenID, serviceID, authName)
	if err != nil {
		t.Fatalf("get fixed binding: %v", err)
	}
	if binding == nil || binding.AuthConnectionID != connectionID || !equalOptionalUUID(binding.ResourceID, resourceID) {
		t.Fatalf("resolved binding = %#v, want connection %s resource %v", binding, connectionID, resourceID)
	}
}

func (fixture appTokenPolicyFixture) assertTokenTransactionRolledBack(t *testing.T, tokenID uuid.UUID) {
	t.Helper()
	var count int
	err := fixture.pool.QueryRow(fixture.ctx, `
		SELECT
			(SELECT COUNT(*) FROM fused_app_token_history WHERE id = $1) +
			(SELECT COUNT(*) FROM fused_app_tokens WHERE id = $1) +
			(SELECT COUNT(*) FROM fused_app_token_bindings WHERE token_id = $1)
	`, tokenID).Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("rolled-back token row count = %d/%v, want 0/nil", count, err)
	}
}

func (fixture appTokenPolicyFixture) seedTokenUsage(t *testing.T, tokenID uuid.UUID) {
	t.Helper()
	_, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_engine_execution_events
			(id, app_family_id, app_id, app_token_id, app_version, transport,
			 direction, endpoint_name, status, latency_ms, started_at, ended_at)
		VALUES ($1, $2, $3, $4, '1.0.0', 'mcp', 'outbound', 'tools/list', 'succeeded', 1, NOW(), NOW())
	`, uuid.New(), fixture.familyID, fixture.appID, tokenID)
	// Execution evidence must exist before the retained-history aggregate is asserted.
	if err != nil {
		t.Fatalf("seed token execution usage: %v", err)
	}
	// Keep the session insert separate because pgx's prepared protocol rejects
	// multiple SQL commands even when both belong to one test fixture setup.
	_, err = fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_mcp_sessions
			(id, app_id, app_token_id, session_id, protocol_version, started_at, last_activity_at)
		VALUES ($1, $2, $3, $4, '2025-06-18', NOW(), NOW())
	`, uuid.New(), fixture.appID, tokenID, "usage-"+tokenID.String())
	// Session evidence independently verifies the second usage aggregate.
	if err != nil {
		t.Fatalf("seed token session usage: %v", err)
	}
}

func (fixture appTokenPolicyFixture) assertTokenUsage(t *testing.T, tokenID uuid.UUID, executionCount, sessionCount int64) {
	t.Helper()
	tokens, err := fixture.repository.ListAppTokens(fixture.ctx, fixture.familyID)
	if err != nil {
		t.Fatalf("list token usage: %v", err)
	}
	for _, token := range tokens {
		if token.ID == tokenID {
			if token.ExecutionCount != executionCount || token.SessionCount != sessionCount || token.LastUsedAt == nil {
				t.Fatalf("token usage = %d/%d/%v", token.ExecutionCount, token.SessionCount, token.LastUsedAt)
			}
			return
		}
	}
	t.Fatalf("token %s missing from retained history", tokenID)
}

func (fixture appTokenPolicyFixture) expireAndAssertRetained(t *testing.T, tokenID uuid.UUID) {
	t.Helper()
	expired, err := fixture.repository.ExpireAppTokens(fixture.ctx, 100)
	if err != nil || expired != 1 {
		t.Fatalf("expire app tokens = %d/%v, want 1/nil", expired, err)
	}
	var status AppTokenStatus
	var reason string
	var terminatedAt *time.Time
	var activeCount int
	err = fixture.pool.QueryRow(fixture.ctx, `
		SELECT history.status, history.termination_reason, history.terminated_at,
		       (SELECT COUNT(*) FROM fused_app_tokens active WHERE active.id = history.id)
		FROM fused_app_token_history history WHERE history.id = $1
	`, tokenID).Scan(&status, &reason, &terminatedAt, &activeCount)
	if err != nil || status != AppTokenStatusExpired || reason != "expired" || terminatedAt == nil || activeCount != 0 {
		t.Fatalf("retained expiry = %s/%q/%v/%d/%v", status, reason, terminatedAt, activeCount, err)
	}
}

type appTokenPolicyFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	repository *postgresStore
	familyID   uuid.UUID
	appID      uuid.UUID
}

func (fixture appTokenPolicyFixture) assertExpansion(t *testing.T, capabilities []string, wantExpansion bool, wantAffected int, label string) {
	t.Helper()
	expands, affected, err := fixture.repository.AssessAppCapabilityExpansion(fixture.ctx, fixture.familyID, capabilities)
	if err != nil {
		t.Fatalf("assess %s: %v", label, err)
	}
	if expands != wantExpansion || affected != wantAffected {
		t.Fatalf("%s = (%v, %d), want (%v, %d)", label, expands, affected, wantExpansion, wantAffected)
	}
}

func assertStrictTokenMetadata(t *testing.T, token *AppTokenMetadata) {
	t.Helper()
	if token.AllowAll || len(token.AllowedOperations) != 2 || token.ExpiresAt == nil || token.BindingMode != AppTokenBindingDynamic {
		t.Fatalf("unexpected created token metadata: %#v", token)
	}
}

func (fixture appTokenPolicyFixture) assertStrictTokenAuthorization(t *testing.T, tokenID uuid.UUID) {
	t.Helper()
	projection, err := fixture.repository.AuthorizeApp(fixture.ctx, fixture.appID, "strict-hash")
	if err != nil {
		t.Fatalf("authorize strict token: %v", err)
	}
	if projection.TokenPolicy.AllowAll || len(projection.TokenPolicy.AllowedOperations) != 2 || projection.TokenID != tokenID || projection.BindingMode != AppTokenBindingDynamic {
		t.Fatalf("unexpected authorization projection: %#v", projection)
	}
}

func (fixture appTokenPolicyFixture) assertAuthorizationDenied(t *testing.T, hash, label string) {
	t.Helper()
	if _, err := fixture.repository.AuthorizeApp(fixture.ctx, fixture.appID, hash); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("%s token authorization error = %v, want %v", label, err, ErrAppNotFound)
	}
}

func (fixture appTokenPolicyFixture) assertListedTokenCount(t *testing.T, want int) {
	t.Helper()
	tokens, err := fixture.repository.ListAppTokens(fixture.ctx, fixture.familyID)
	if err != nil {
		t.Fatalf("list app tokens: %v", err)
	}
	if len(tokens) != want {
		t.Fatalf("listed %d tokens, want %d", len(tokens), want)
	}
}

func (fixture appTokenPolicyFixture) revokeAndAssertDenied(t *testing.T, tokenID uuid.UUID, name, hash string) {
	t.Helper()
	revocation, err := fixture.repository.RevokeAppToken(fixture.ctx, fixture.familyID, name)
	if err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	if revocation.TokenID != tokenID || revocation.AppFamilyID != fixture.familyID || revocation.RevokedAt.IsZero() {
		t.Fatalf("revocation projection = %#v", revocation)
	}
	fixture.assertAuthorizationDenied(t, hash, "revoked")
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
	token, err := fixture.repository.CreateAppToken(fixture.ctx, AppTokenIssue{
		ID: uuid.New(), AppFamilyID: fixture.familyID, TokenHash: name + "-hash",
		Name: name, Policy: policy, BindingMode: AppTokenBindingDynamic,
	})
	if err != nil {
		t.Fatalf("create %q app token: %v", name, err)
	}
	return token
}
