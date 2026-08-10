package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEngineSchemaDefinesAccessControlFoundation(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_subjects",
		"CHECK (kind IN ('bootstrap', 'user', 'service_account', 'app'))",
		"CHECK (status IN ('invited', 'active', 'suspended', 'archived'))",
		"uq_fused_subjects_bootstrap",
		"CREATE TABLE IF NOT EXISTS fused_teams",
		"CREATE TABLE IF NOT EXISTS fused_team_memberships",
		"idx_fused_team_memberships_member",
		"CREATE TABLE IF NOT EXISTS fused_control_credentials",
		"key_hash     text NOT NULL UNIQUE",
		"source       text NOT NULL DEFAULT 'api_key'",
		"auth_method  text NOT NULL DEFAULT 'api_key'",
		"idx_fused_control_credentials_active_hash",
		"WHERE revoked_at IS NULL",
		"CREATE TABLE IF NOT EXISTS fused_cli_login_transactions",
		"browser_secret_hash      text NOT NULL UNIQUE",
		"credential_hash          text NOT NULL UNIQUE",
		"idx_fused_cli_login_transactions_expiry",
		"CREATE TABLE IF NOT EXISTS fused_external_identities",
		"PRIMARY KEY (issuer, external_subject)",
		"CONSTRAINT uq_fused_external_identity_user UNIQUE (user_subject_id)",
		"CREATE TABLE IF NOT EXISTS fused_managed_login_transactions",
		"poll_secret_hash             text NOT NULL UNIQUE",
		"encrypted_registry_verifier  text",
		"idx_fused_managed_login_transactions_expiry",
		"CREATE TABLE IF NOT EXISTS fused_browser_logout_contexts",
		"encrypted_logout_token text NOT NULL",
		"idx_fused_browser_logout_contexts_expiry",
		"CREATE TABLE IF NOT EXISTS fused_roles",
		"CREATE TABLE IF NOT EXISTS fused_role_permissions",
		"CREATE TABLE IF NOT EXISTS fused_role_bindings",
		"UNIQUE (subject_type, subject_id, role_id, resource_type, resource_id)",
		"idx_fused_role_bindings_subject_scope",
		"CREATE TABLE IF NOT EXISTS fused_authorization_state",
		"CHECK (singleton_key = 1)",
		"CREATE TABLE IF NOT EXISTS fused_audit_events",
		"CHECK (outcome IN ('attempted', 'allowed', 'denied', 'succeeded', 'failed'))",
		"missing_requirements jsonb NOT NULL DEFAULT '[]'::jsonb",
		"metadata            jsonb NOT NULL DEFAULT '{}'::jsonb",
		"idx_fused_audit_events_occurred_at",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected access-control schema containing %q", expected)
		}
	}
	if strings.Contains(joined, "ADD COLUMN IF NOT EXISTS missing_requirements") {
		t.Fatal("missing_requirements must be defined by the clean-cutover table schema")
	}
}

func TestEngineMigrationsAddManagedLogoutHandoffColumns(t *testing.T) {
	joined := strings.Join(engineMigrationQueries(), "\n")
	for _, expected := range []string{
		"ALTER TABLE fused_managed_login_transactions ADD COLUMN IF NOT EXISTS logout_encrypted_dek text",
		"ALTER TABLE fused_managed_login_transactions ADD COLUMN IF NOT EXISTS encrypted_logout_token text",
		"ALTER TABLE fused_managed_login_transactions ADD COLUMN IF NOT EXISTS logout_expires_at timestamptz",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected Engine migration containing %q", expected)
		}
	}
}

func TestEngineSchemaCreatesAccessControlDependenciesInOrder(t *testing.T) {
	queries := engineSchemaQueries()
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_subjects", "CREATE TABLE IF NOT EXISTS fused_control_credentials")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_control_credentials", "CREATE TABLE IF NOT EXISTS fused_cli_login_transactions")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_control_credentials", "CREATE TABLE IF NOT EXISTS fused_browser_logout_contexts")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_users", "CREATE TABLE IF NOT EXISTS fused_external_identities")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_users", "CREATE TABLE IF NOT EXISTS fused_managed_login_transactions")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_subjects", "CREATE TABLE IF NOT EXISTS fused_team_memberships")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_teams", "CREATE TABLE IF NOT EXISTS fused_team_memberships")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_subjects", "CREATE TABLE IF NOT EXISTS fused_audit_events")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_control_credentials", "CREATE TABLE IF NOT EXISTS fused_audit_events")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_roles", "CREATE TABLE IF NOT EXISTS fused_role_permissions")
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_roles", "CREATE TABLE IF NOT EXISTS fused_role_bindings")
}

func TestEngineAccessControlSchemaInitializationIsIdempotent(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()

	firstMigration := readEngineMigrationRecord(t, ctx, pool)

	// Base schema reconciliation remains idempotent, while the ledger ensures the
	// transactional migration batch is not replayed on every restart.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("second initEngineSchema: %v", err)
	}

	assertEngineMigrationNotReplayed(t, ctx, pool, firstMigration)
	assertAccessControlTablesExist(t, ctx, pool)
	assertAuthorizationStateSingleton(t, ctx, pool)
}

type engineMigrationRecord struct {
	Name      string
	AppliedAt time.Time
}

func readEngineMigrationRecord(t *testing.T, ctx context.Context, pool *pgxpool.Pool) engineMigrationRecord {
	t.Helper()
	var record engineMigrationRecord
	if err := pool.QueryRow(ctx, `SELECT name, applied_at FROM fused_engine_schema_migrations WHERE version = $1`, engineMigrationVersion).Scan(&record.Name, &record.AppliedAt); err != nil {
		t.Fatalf("read Engine migration ledger entry: %v", err)
	}
	return record
}

func assertEngineMigrationNotReplayed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, first engineMigrationRecord) {
	t.Helper()
	var migrationRows int
	var second engineMigrationRecord
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), MIN(name), MIN(applied_at) FROM fused_engine_schema_migrations`).Scan(&migrationRows, &second.Name, &second.AppliedAt); err != nil {
		t.Fatalf("read Engine migration ledger after restart: %v", err)
	}
	if migrationRows != len(engineMigrations()) {
		t.Fatalf("Engine migration ledger rows = %d, want %d", migrationRows, len(engineMigrations()))
	}
	if first.Name != engineMigrationName || second.Name != first.Name {
		t.Fatalf("Engine migration name changed across restart: first=%q second=%q", first.Name, second.Name)
	}
	if !second.AppliedAt.Equal(first.AppliedAt) {
		t.Fatalf("Engine migration replayed: first=%s second=%s", first.AppliedAt, second.AppliedAt)
	}
}

func assertAccessControlTablesExist(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{
		"fused_subjects",
		"fused_control_credentials",
		"fused_cli_login_transactions",
		"fused_external_identities",
		"fused_managed_login_transactions",
		"fused_browser_logout_contexts",
		"fused_teams",
		"fused_team_memberships",
		"fused_roles",
		"fused_role_permissions",
		"fused_role_bindings",
		"fused_authorization_state",
		"fused_audit_events",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %s to exist", table)
		}
	}
}

func assertAuthorizationStateSingleton(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var stateRows int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM fused_authorization_state").Scan(&stateRows); err != nil {
		t.Fatalf("count authorization state: %v", err)
	}
	if stateRows != 1 {
		t.Fatalf("authorization state rows = %d, want 1", stateRows)
	}
}

func assertSchemaOrder(t *testing.T, queries []string, first, second string) {
	t.Helper()
	firstIndex := indexOfSchemaFragment(queries, first)
	secondIndex := indexOfSchemaFragment(queries, second)
	if firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("schema fragments missing: first=%q (%d), second=%q (%d)", first, firstIndex, second, secondIndex)
	}
	if firstIndex >= secondIndex {
		t.Fatalf("schema fragment %q must precede %q", first, second)
	}
}
