package store

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/db"
)

const authorizationBenchmarkBindingCount = 100

type authorizationBenchmarkFixture struct {
	repository   *postgresStore
	credential   string
	revision     int64
	requirements []accesscontrol.Requirement
}

func BenchmarkAuthorizationAcceptance(b *testing.B) {
	pool := newAuthorizationBenchmarkPool(b)
	for _, membershipCount := range []int{1, 10, 50} {
		name := "memberships_" + strconv.Itoa(membershipCount) + "_bindings_" + strconv.Itoa(authorizationBenchmarkBindingCount)
		b.Run("cold_auth/"+name, func(b *testing.B) {
			fixture := newAuthorizationBenchmarkFixture(b, pool, membershipCount, authorizationBenchmarkBindingCount)
			benchmarkPostgresColdAuthentication(b, fixture)
		})
		b.Run("cache_hit/"+name, func(b *testing.B) {
			fixture := newAuthorizationBenchmarkFixture(b, pool, membershipCount, authorizationBenchmarkBindingCount)
			benchmarkPostgresCacheHit(b, fixture)
		})
		b.Run("authorization_check_all/"+name, func(b *testing.B) {
			fixture := newAuthorizationBenchmarkFixture(b, pool, membershipCount, authorizationBenchmarkBindingCount)
			benchmarkPostgresCheckAll(b, fixture)
		})
	}
}

func benchmarkPostgresColdAuthentication(b *testing.B, fixture authorizationBenchmarkFixture) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		// A fresh authenticator deliberately bypasses the credential cache while
		// exercising the production repository query and snapshot construction.
		authenticator, err := accesscontrol.NewAuthenticator(fixture.repository, fixture.revision, accesscontrol.AuthenticatorOptions{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := authenticator.AuthenticateControlCredential(context.Background(), fixture.credential); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkPostgresCacheHit(b *testing.B, fixture authorizationBenchmarkFixture) {
	b.Helper()
	authenticator, err := accesscontrol.NewAuthenticator(fixture.repository, fixture.revision, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := authenticator.AuthenticateControlCredential(context.Background(), fixture.credential); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := authenticator.AuthenticateControlCredential(context.Background(), fixture.credential); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkPostgresCheckAll(b *testing.B, fixture authorizationBenchmarkFixture) {
	b.Helper()
	authenticator, err := accesscontrol.NewAuthenticator(fixture.repository, fixture.revision, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		b.Fatal(err)
	}
	actor, err := authenticator.AuthenticateControlCredential(context.Background(), fixture.credential)
	if err != nil {
		b.Fatal(err)
	}
	authorizer := accesscontrol.SnapshotAuthorizer{}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := authorizer.CheckAll(context.Background(), actor, fixture.requirements...); err != nil {
			b.Fatal(err)
		}
	}
}

func newAuthorizationBenchmarkPool(b *testing.B) *pgxpool.Pool {
	b.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		b.Skip("DATABASE_URL is required; use a dedicated benchmark database")
	}
	if os.Getenv("FUSED_BENCHMARK_ALLOW_DB_RESET") != "1" {
		b.Skip("FUSED_BENCHMARK_ALLOW_DB_RESET=1 is required to reset the dedicated benchmark database")
	}
	pool, err := db.InitEnginePostgres(context.Background(), databaseURL)
	if err != nil {
		b.Fatalf("initialize benchmark database: %v", err)
	}
	b.Cleanup(pool.Close)
	return pool
}

func newAuthorizationBenchmarkFixture(b *testing.B, pool *pgxpool.Pool, membershipCount, bindingCount int) authorizationBenchmarkFixture {
	b.Helper()
	resetAuthorizationBenchmarkDatabase(b, pool)
	repository := NewPostgresStore(pool).(*postgresStore)
	accountID := uuid.New()
	if _, err := repository.BootstrapWorkspace(context.Background(), accountID, "Authorization Benchmark"); err != nil {
		b.Fatalf("bootstrap workspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(context.Background(), repository, accountID, "fsk_benchmark_owner")
	if err != nil {
		b.Fatalf("bootstrap roles: %v", err)
	}
	credential := "fsk_benchmark_member_" + uuid.NewString()
	credentialHash := accesscontrol.HashControlCredential(credential)
	userID := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		WITH subject AS (
			INSERT INTO fused_subjects (id, kind, display_name, status)
			VALUES ($1, 'user', 'Benchmark Member', 'active')
		)
		INSERT INTO fused_control_credentials (subject_id, key_hash, key_prefix, name)
		VALUES ($1, $2, 'fsk_benc', 'authorization benchmark')
	`, userID, credentialHash); err != nil {
		b.Fatalf("insert benchmark subject: %v", err)
	}
	teamIDs, teamSlugs := benchmarkTeams(membershipCount)
	resourceIDs := benchmarkResourceIDs(bindingCount)
	if _, err := pool.Exec(context.Background(), `
		WITH teams AS (
			INSERT INTO fused_teams (id, name, slug)
			SELECT id, slug, slug FROM unnest($1::uuid[], $2::text[]) AS input(id, slug)
		), memberships AS (
			INSERT INTO fused_team_memberships (team_id, member_subject_id)
			SELECT id, $3 FROM unnest($1::uuid[]) AS input(id)
		)
		INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id)
		SELECT 'team', input.team_id, role.id, 'service', input.resource_id
		FROM unnest($4::uuid[], $5::uuid[]) AS input(team_id, resource_id)
		CROSS JOIN fused_roles role
		WHERE role.slug = 'service-user'
	`, teamIDs, teamSlugs, userID, bindingTeamIDs(teamIDs, bindingCount), resourceIDs); err != nil {
		b.Fatalf("insert benchmark memberships and bindings: %v", err)
	}
	principal, err := repository.LoadControlPrincipal(context.Background(), credentialHash)
	if err != nil {
		b.Fatalf("verify benchmark principal: %v", err)
	}
	if len(principal.EffectiveGrants) < bindingCount {
		b.Fatalf("effective grants = %d, want at least %d", len(principal.EffectiveGrants), bindingCount)
	}
	return authorizationBenchmarkFixture{
		repository: repository, credential: credential, revision: owner.Revision,
		requirements: benchmarkRequirements(resourceIDs),
	}
}

func benchmarkTeams(count int) ([]uuid.UUID, []string) {
	ids := make([]uuid.UUID, count)
	slugs := make([]string, count)
	for index := range ids {
		ids[index] = uuid.New()
		slugs[index] = "benchmark-" + uuid.NewString()
	}
	return ids, slugs
}

func benchmarkResourceIDs(count int) []uuid.UUID {
	ids := make([]uuid.UUID, count)
	for index := range ids {
		ids[index] = uuid.New()
	}
	return ids
}

func bindingTeamIDs(teamIDs []uuid.UUID, count int) []uuid.UUID {
	ids := make([]uuid.UUID, count)
	for index := range ids {
		ids[index] = teamIDs[index%len(teamIDs)]
	}
	return ids
}

func benchmarkRequirements(resourceIDs []uuid.UUID) []accesscontrol.Requirement {
	requirements := make([]accesscontrol.Requirement, len(resourceIDs))
	for index, resourceID := range resourceIDs {
		requirements[index] = accesscontrol.Requirement{
			Permission: accesscontrol.PermissionServiceConsume,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: resourceID},
		}
	}
	return requirements
}

func resetAuthorizationBenchmarkDatabase(b *testing.B, pool *pgxpool.Pool) {
	b.Helper()
	// Benchmarks intentionally reset access-control state. Documentation requires
	// an isolated database so local development data cannot be mistaken for a fixture.
	if _, err := pool.Exec(context.Background(), `
		DELETE FROM fused_config_plans;
		DELETE FROM fused_config_states;
		DELETE FROM fused_app_tokens;
		DELETE FROM fused_app_family_buckets;
		DELETE FROM fused_apps;
		DELETE FROM fused_app_tombstones;
		DELETE FROM fused_app_families;
		DELETE FROM fused_audit_events;
		DELETE FROM fused_role_bindings;
		DELETE FROM fused_role_permissions;
		DELETE FROM fused_control_credentials;
		DELETE FROM fused_team_memberships;
		DELETE FROM fused_teams;
		DELETE FROM fused_roles;
		DELETE FROM fused_users;
		DELETE FROM fused_subjects;
		DELETE FROM fused_workspaces;
		UPDATE fused_authorization_state SET revision = 1, updated_at = NOW() WHERE singleton_key = 1;
	`); err != nil {
		b.Fatalf("reset benchmark database: %v", err)
	}
}
