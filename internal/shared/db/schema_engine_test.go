package db

import (
	"strings"
	"testing"
)

// engineSchemaTable extracts one canonical CREATE TABLE statement for focused invariant checks.
func engineSchemaTable(t *testing.T, tableName string) string {
	t.Helper()
	schema := strings.Join(engineSchemaQueries(), "\n")
	marker := "CREATE TABLE IF NOT EXISTS " + tableName + " ("
	start := strings.Index(schema, marker)
	// Missing tables must fail explicitly instead of returning a neighboring definition.
	if start < 0 {
		t.Fatalf("Engine schema table %q is missing", tableName)
	}
	remainder := schema[start:]
	end := strings.Index(remainder, ");")
	// Unterminated definitions cannot be inspected safely.
	if end < 0 {
		t.Fatalf("Engine schema table %q is unterminated", tableName)
	}
	return remainder[:end+2]
}

// assertSchemaContainsAll keeps related schema fragments readable in table-driven assertions.
func assertSchemaContainsAll(t *testing.T, schema, failureFormat string, expectedFragments []string) {
	t.Helper()
	for _, expected := range expectedFragments {
		// Every listed fragment is part of the current persistence contract.
		if !strings.Contains(schema, expected) {
			t.Fatalf(failureFormat, expected)
		}
	}
}

// indexOfSchemaFragment locates ordering-sensitive schema statements without executing PostgreSQL.
func indexOfSchemaFragment(queries []string, fragment string) int {
	for index, query := range queries {
		// Schema order matters when later constraints reference earlier tables.
		if strings.Contains(query, fragment) {
			return index
		}
	}
	return -1
}

// TestEngineSchemaUsesCleanBaseline proves numbered migrations and their retired table are no longer bootstrapped.
func TestEngineSchemaUsesCleanBaseline(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	for _, forbidden := range []string{
		"fused_engine_schema_migrations",
		"fused_workspace_auth_references",
		"chk_fused_connect_sessions_service_version",
	} {
		// A clean baseline must not recreate or preserve historical admission paths.
		if strings.Contains(schema, forbidden) {
			t.Fatalf("clean Engine schema retained %q", forbidden)
		}
	}
	assertSchemaContainsAll(t, schema, "sensitive cutover ledger missing %q", []string{
		"CREATE TABLE IF NOT EXISTS fused_sensitive_data_migrations",
		"rows_migrated bigint NOT NULL DEFAULT 0",
		"completed_at timestamptz NOT NULL DEFAULT NOW()",
	})
}

// TestEngineSchemaDefinesStableMCPFamilyRouting locks the direct columns,
// one-time backfill marker, and referential cleanup into the clean baseline.
func TestEngineSchemaDefinesStableMCPFamilyRouting(t *testing.T) {
	families := engineSchemaTable(t, "fused_app_families")
	assertSchemaContainsAll(t, families, "stable MCP family schema missing %q", []string{
		"mcp_stable_app_id       uuid",
		"mcp_stable_route_initialized boolean NOT NULL DEFAULT false",
		"chk_fused_app_families_stable_mcp",
		"kind = 'sdk' AND mcp_stable_app_id IS NULL AND NOT mcp_stable_route_initialized",
		"kind = 'mcp' AND (mcp_stable_app_id IS NULL OR mcp_stable_route_initialized)",
	})
	schema := strings.Join(engineSchemaQueries(), "\n")
	assertSchemaContainsAll(t, schema, "stable MCP convergence missing %q", []string{
		"ALTER TABLE fused_app_families ADD COLUMN IF NOT EXISTS mcp_stable_app_id uuid",
		"ALTER TABLE fused_app_families ADD COLUMN IF NOT EXISTS mcp_stable_route_initialized boolean NOT NULL DEFAULT false",
		"ORDER BY app.activated_at DESC NULLS LAST, app.created_at DESC, app.app_id DESC",
		"AND NOT family.mcp_stable_route_initialized",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_apps_app_family_identity",
		"FOREIGN KEY (mcp_stable_app_id, app_family_id)",
		"ON DELETE SET NULL (mcp_stable_app_id)",
	})
	// Stable-route convergence validates existing data directly; it must not
	// reintroduce the deferred constraint state removed from older migrations.
	if strings.Contains(schema, "fk_fused_app_families_stable_mcp NOT VALID") {
		t.Fatal("stable MCP foreign key retained a deferred validation state")
	}
}

// TestEngineSchemaRequiresExactConnectedAuthVersions removes the v8 nullable-version compatibility state.
func TestEngineSchemaRequiresExactConnectedAuthVersions(t *testing.T) {
	for name, table := range map[string]string{
		"auth connections": engineSchemaTable(t, "fused_auth_connections"),
		"connect sessions": engineSchemaTable(t, "fused_connect_sessions"),
	} {
		assertSchemaContainsAll(t, table, name+" schema missing %q", []string{
			"service_version_id uuid NOT NULL",
			"credential_source_service_id uuid NOT NULL",
			"credential_source_auth_type text NOT NULL",
			"credential_source_auth_name text NOT NULL",
		})
	}
	schema := strings.Join(engineSchemaQueries(), "\n")
	assertSchemaContainsAll(t, schema, "exact-version convergence missing %q", []string{
		"ALTER TABLE fused_auth_connections ALTER COLUMN service_version_id SET NOT NULL",
		"ALTER TABLE fused_connect_sessions ALTER COLUMN service_version_id SET NOT NULL",
		"COALESCE(LEAST(expires_at, refresh_token_expires_at), expires_at, refresh_token_expires_at)",
		"oauth2_authorization_code",
		"open_id_connect",
	})
	// A NOT NULL column must not retain a partial-index branch for unsupported legacy grants.
	if strings.Contains(schema, "AND service_version_id IS NOT NULL") {
		t.Fatal("connected-auth refresh index retained nullable-version compatibility")
	}
}

// TestEngineSchemaDefinesIsolatedConnectInputSessions locks pre-OAuth state to a one-time token without callback secrets.
func TestEngineSchemaDefinesIsolatedConnectInputSessions(t *testing.T) {
	table := engineSchemaTable(t, "fused_connect_input_sessions")
	assertSchemaContainsAll(t, table, "connect input session schema missing %q", []string{
		"token_hash         text NOT NULL UNIQUE",
		"contract_hash      text NOT NULL CHECK",
		"expires_at         timestamptz NOT NULL",
		"used_at            timestamptz",
		"credential_source_service_id uuid NOT NULL",
	})
	for _, forbidden := range []string{"state_hash", "nonce_hash", "pkce_verifier", "encrypted_dek"} {
		// OAuth callback material belongs only to the post-input session.
		if strings.Contains(table, forbidden) {
			t.Fatalf("connect input session schema contains OAuth field %q", forbidden)
		}
	}
}

// TestEngineSchemaDefinesCurrentMCPAndActivityShape folds former additive columns into canonical tables.
func TestEngineSchemaDefinesCurrentMCPAndActivityShape(t *testing.T) {
	mcp := engineSchemaTable(t, "fused_mcp_sessions")
	events := engineSchemaTable(t, "fused_engine_execution_events")
	assertSchemaContainsAll(t, mcp, "MCP session schema missing %q", []string{
		"app_id uuid NOT NULL", "app_token_id uuid", "client_name text NOT NULL DEFAULT ''",
		"client_version text NOT NULL DEFAULT ''", "initial_client_ip inet", "tool_call_timeout",
	})
	assertSchemaContainsAll(t, events, "execution activity schema missing %q", []string{
		"execution_kind text NOT NULL DEFAULT 'physical'", "parent_execution_id uuid",
		"unified_target text NOT NULL DEFAULT ''", "execution_phase text NOT NULL DEFAULT ''",
		"unified_steps jsonb NOT NULL DEFAULT '[]'", "transport NOT IN ('sdk', 'mcp', 'rest')",
	})
	schema := strings.Join(engineSchemaQueries(), "\n")
	assertSchemaContainsAll(t, schema, "runtime activity index missing %q", []string{
		"idx_fused_mcp_sessions_cursor", "idx_fused_mcp_sessions_token_started",
		"idx_fused_execution_parent", "idx_fused_engine_execution_events_token_started",
	})
}

// TestEngineSchemaDefinesImmutableUnifiedOperations keeps v3 direct creation plus its narrow live convergence.
func TestEngineSchemaDefinesImmutableUnifiedOperations(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	convergence := strings.Join(unifiedSchemaConvergenceQueries(), "\n")
	assertSchemaContainsAll(t, schema, "Unified app schema missing %q", []string{
		"unified_definition_schema_version integer NOT NULL DEFAULT 3",
		"unified_definition_schema_version = 3",
		"unified_definitions    jsonb NOT NULL DEFAULT '[]'::jsonb",
		"CONSTRAINT chk_fused_apps_unified_definition_shape",
		"CONSTRAINT chk_fused_apps_unified_hashes",
	})
	assertSchemaContainsAll(t, convergence, "Unified convergence missing %q", []string{
		"ADD COLUMN IF NOT EXISTS unified_definition_schema_version integer NOT NULL DEFAULT 3",
		"ADD CONSTRAINT chk_fused_apps_unified_definition_shape",
		"ADD CONSTRAINT chk_fused_apps_unified_hashes",
		"NOT VALID",
		"VALIDATE CONSTRAINT chk_fused_apps_unified_definition_shape",
	})
	for _, forbidden := range []string{"UPDATE fused_apps", "IN (2, 3)", "schema v1", "schema v2"} {
		// Convergence may constrain old rows but must never relabel their bytes.
		if strings.Contains(convergence, forbidden) {
			t.Fatalf("Unified convergence retained draft promotion %q", forbidden)
		}
	}
}

// TestEngineSchemaDefinesAppTokenLifecycle keeps executable hashes separate from durable token evidence.
func TestEngineSchemaDefinesAppTokenLifecycle(t *testing.T) {
	history := engineSchemaTable(t, "fused_app_token_history")
	active := engineSchemaTable(t, "fused_app_tokens")
	bindings := engineSchemaTable(t, "fused_app_token_bindings")
	assertSchemaContainsAll(t, history, "token history schema missing %q", []string{
		"issued_by_subject_id", "issued_by_credential_id", "status", "terminated_at", "termination_reason", "binding_mode",
	})
	// Durable history must never retain an executable credential hash.
	if strings.Contains(history, "token_hash") {
		t.Fatal("token history must not retain an executable credential hash")
	}
	assertSchemaContainsAll(t, active, "active token schema missing %q", []string{
		"token_hash", "allow_all", "allowed_operations", "binding_mode", "fk_fused_app_tokens_history",
	})
	assertSchemaContainsAll(t, bindings, "token binding schema missing %q", []string{
		"auth_connection_id", "resource_id", "PRIMARY KEY (token_id, service_id, auth_name)",
	})
}

// TestEngineSchemaDefinesCurrentProductTablesDirectly protects clean app, config, webhook, and contract ownership.
func TestEngineSchemaDefinesCurrentProductTablesDirectly(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	assertSchemaContainsAll(t, schema, "current Engine schema missing %q", []string{
		"UNIQUE (account_id, kind, canonical_name)", "UNIQUE (app_family_id, version)",
		"(owner_subject_id IS NOT NULL)::int + (owner_team_id IS NOT NULL)::int = 1",
		"required_permissions jsonb NOT NULL", "owning_config_key     text NOT NULL CHECK (owning_config_key <> '')",
		"secret_bucket_id      uuid REFERENCES fused_buckets(id) ON DELETE RESTRICT",
		"contract_version   integer NOT NULL", "required_capabilities text[] NOT NULL",
		"generation_contract_hash text NOT NULL DEFAULT ''", "timeout_ms              integer",
		"CHECK (timeout_ms IS NULL OR timeout_ms BETWEEN 1 AND 86400000)",
		"CHECK (severity IN ('breaking', 'non-breaking'))",
		"bucket_id        uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE",
		"CONSTRAINT uq_workspace_secrets UNIQUE (bucket_id, service_id, key_name)",
		"CREATE INDEX IF NOT EXISTS idx_fused_workspace_secrets_lookup",
		"app_id uuid NOT NULL REFERENCES fused_apps(app_id) ON DELETE CASCADE",
		"CONSTRAINT chk_fused_engine_idempotency_response_media_family",
	})
	for _, table := range []string{
		"fused_artifact_scopes", "fused_artifact_tokens", "fused_artifact_buckets",
		"fused_artifact_snapshots", "fused_app_family_migrations", "fused_bucket_bindings",
		"fused_bucket_profile_attachments",
	} {
		// Retired persistence must not return through the clean baseline.
		if strings.Contains(schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Fatalf("clean Engine schema creates retired table %s", table)
		}
	}
}

// TestEngineSchemaDefinesAccessAndRuntimeFoundations checks the remaining cross-cutting contracts.
func TestEngineSchemaDefinesAccessAndRuntimeFoundations(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	assertSchemaContainsAll(t, schema, "Engine foundation schema missing %q", []string{
		"CREATE TABLE IF NOT EXISTS fused_engine_installation",
		"installation_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid()",
		"subject_type IN ('subject', 'team', 'workspace')",
		"'attempted', 'allowed', 'denied', 'succeeded', 'failed', 'rolled_back', 'cancelled'",
		"CREATE TABLE IF NOT EXISTS fused_runtime_entitlements",
		"CREATE TABLE IF NOT EXISTS fused_engine_usage_counter_reports",
		"CREATE TABLE IF NOT EXISTS fused_provider_rate_limit_states",
		"rate_limit_unit_totals bigint[] NOT NULL DEFAULT '{}'",
		"CREATE TABLE IF NOT EXISTS fused_workspace_connection_profiles",
		"CREATE TABLE IF NOT EXISTS fused_workspace_connection_bindings",
	})
}

// TestEngineSchemaCreatesMCPAfterIdentityTargets protects direct foreign-key bootstrap ordering.
func TestEngineSchemaCreatesMCPAfterIdentityTargets(t *testing.T) {
	queries := engineSchemaQueries()
	apps := indexOfSchemaFragment(queries, "CREATE TABLE IF NOT EXISTS fused_apps")
	tokenHistory := indexOfSchemaFragment(queries, "CREATE TABLE IF NOT EXISTS fused_app_token_history")
	idempotency := indexOfSchemaFragment(queries, "CREATE TABLE IF NOT EXISTS fused_engine_idempotency_keys")
	mcp := indexOfSchemaFragment(queries, "CREATE TABLE IF NOT EXISTS fused_mcp_sessions")
	// Both referenced tables must exist before PostgreSQL admits the late constraints.
	if apps < 0 || tokenHistory < 0 || idempotency <= apps || mcp <= apps || mcp <= tokenHistory {
		t.Fatalf("runtime identity order = apps:%d history:%d idempotency:%d sessions:%d", apps, tokenHistory, idempotency, mcp)
	}
	assertSchemaContainsAll(t, engineSchemaTable(t, "fused_mcp_sessions"), "MCP foreign key missing %q", []string{
		"app_id uuid NOT NULL REFERENCES fused_apps(app_id) ON DELETE CASCADE",
		"app_token_id uuid REFERENCES fused_app_token_history(id) ON DELETE SET NULL",
	})
}
