package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEngineSchemaDefinesAccessControlFoundation(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_subjects",
		"CHECK (kind IN ('bootstrap', 'user', 'service_account', 'artifact'))",
		"CHECK (status IN ('invited', 'active', 'suspended', 'archived'))",
		"uq_fused_subjects_bootstrap",
		"CREATE TABLE IF NOT EXISTS fused_teams",
		"CREATE TABLE IF NOT EXISTS fused_team_memberships",
		"idx_fused_team_memberships_member",
		"CREATE TABLE IF NOT EXISTS fused_control_credentials",
		"key_hash     text NOT NULL UNIQUE",
		"idx_fused_control_credentials_active_hash",
		"WHERE revoked_at IS NULL",
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

func TestEngineSchemaCreatesAccessControlDependenciesInOrder(t *testing.T) {
	queries := engineSchemaQueries()
	assertSchemaOrder(t, queries, "CREATE TABLE IF NOT EXISTS fused_subjects", "CREATE TABLE IF NOT EXISTS fused_control_credentials")
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

	// Replaying initialization is how every Engine restart upgrades an existing
	// installation, so each statement must converge without manual migration state.
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("second initEngineSchema: %v", err)
	}

	for _, table := range []string{
		"fused_subjects",
		"fused_control_credentials",
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
