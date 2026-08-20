package db

import (
	"strings"
	"testing"
)

// TestEngineSchemaDefinesIsolatedConnectInputSessions locks the pre-OAuth form
// session to a hashed, expiring, one-time token without callback credentials.
func TestEngineSchemaDefinesIsolatedConnectInputSessions(t *testing.T) {
	table := engineSchemaTable(t, "fused_connect_input_sessions")
	for _, expected := range []string{
		"token_hash         text NOT NULL UNIQUE",
		"contract_hash      text NOT NULL CHECK",
		"expires_at         timestamptz NOT NULL",
		"used_at            timestamptz",
	} {
		// The clean schema must directly encode one-time token storage and expiry.
		if !strings.Contains(table, expected) {
			t.Fatalf("connect input session schema missing %q", expected)
		}
	}
	for _, forbidden := range []string{
		"state_hash",
		"nonce_hash",
		"pkce_verifier",
		"encrypted_dek",
	} {
		// OAuth callback material belongs only to the session created after form completion.
		if strings.Contains(table, forbidden) {
			t.Fatalf("connect input session schema contains OAuth field %q", forbidden)
		}
	}
}

// engineSchemaTable extracts one clean-schema CREATE TABLE statement so
// forbidden-field checks do not match neighboring tables with different duties.
func engineSchemaTable(t *testing.T, tableName string) string {
	t.Helper()
	schema := strings.Join(engineSchemaQueries(), "\n")
	marker := "CREATE TABLE IF NOT EXISTS " + tableName + " ("
	start := strings.Index(schema, marker)
	// A missing table is reported explicitly instead of returning an unrelated slice.
	if start < 0 {
		t.Fatalf("Engine schema table %q is missing", tableName)
	}
	remainder := schema[start:]
	end := strings.Index(remainder, ");")
	// An unterminated definition indicates the clean schema cannot be inspected safely.
	if end < 0 {
		t.Fatalf("Engine schema table %q is unterminated", tableName)
	}
	return remainder[:end+2]
}

func TestEngineSchemaDefinesStableInstallationIdentity(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"CREATE TABLE IF NOT EXISTS fused_engine_installation",
		"installation_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid()",
		"CHECK (singleton_key = 1)",
		"ON CONFLICT (singleton_key) DO NOTHING",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected stable Engine installation schema containing %q", expected)
		}
	}
}

// TestEngineSchemaDefinesVersionedMigrationLedger locks migration identities so
// an already-recorded version can never silently acquire different queries.
func TestEngineSchemaDefinesVersionedMigrationLedger(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"CREATE TABLE IF NOT EXISTS fused_engine_schema_migrations",
		"version    bigint PRIMARY KEY",
		"name       text NOT NULL UNIQUE",
		"applied_at timestamptz NOT NULL DEFAULT NOW()",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected Engine migration ledger containing %q", expected)
		}
	}

	migrations := engineMigrations()
	if len(migrations) != 9 {
		t.Fatalf("Engine migration count = %d, want 9", len(migrations))
	}
	assertMigrationIdentity(t, migrations[0], engineMigrationVersion, engineMigrationName)
	assertMigrationIdentity(t, migrations[1], appTokenPolicyMigrationVersion, appTokenPolicyMigrationName)
	assertMigrationIdentity(t, migrations[2], contractEnvelopeMigrationVersion, contractEnvelopeMigrationName)
	assertMigrationIdentity(t, migrations[3], idempotencyMediaMigrationVersion, idempotencyMediaMigrationName)
	assertMigrationIdentity(t, migrations[4], connectBrandingMigrationVersion, connectBrandingMigrationName)
	assertMigrationIdentity(t, migrations[5], connectBrandColorMigrationVersion, connectBrandColorMigrationName)
	assertMigrationIdentity(t, migrations[6], connectBrandVioletMigrationVersion, connectBrandVioletMigrationName)
	assertMigrationIdentity(t, migrations[7], managedOAuthRefreshMigrationVersion, managedOAuthRefreshMigrationName)
	assertMigrationIdentity(t, migrations[8], restExecutionMigrationVersion, restExecutionMigrationName)
	if engineMigrationLockQuery != "SELECT pg_advisory_xact_lock($1)" {
		t.Fatalf("Engine migrations must use a transaction-scoped advisory lock, got %q", engineMigrationLockQuery)
	}
}

// TestRESTExecutionTransportOwnsOnlyMigrationNine protects the immutable v1
// receipt constraint while ensuring fresh and upgraded databases accept REST.
func TestRESTExecutionTransportOwnsOnlyMigrationNine(t *testing.T) {
	fresh := schemaQueryContaining(engineSchemaQueries(), "CREATE TABLE IF NOT EXISTS fused_engine_execution_events")
	v1 := strings.Join(engineMigrationV1Queries(), "\n")
	v9 := strings.Join(restExecutionMigrationQueries(), "\n")
	if !strings.Contains(fresh, "('sdk', 'mcp', 'rest')") {
		t.Fatal("fresh execution receipt constraint does not include REST")
	}
	if strings.Contains(v1, "'rest'") {
		t.Fatal("immutable migration v1 was changed to include REST")
	}
	if !strings.Contains(v9, "('sdk', 'mcp', 'rest')") {
		t.Fatal("migration v9 does not own REST receipt constraint widening")
	}
}

// TestEngineSchemaDefinesConnectBrandingDefaults keeps fresh databases aligned
// with the Engine chrome while preserving the immutable earlier migration shapes.
func TestEngineSchemaDefinesConnectBrandingDefaults(t *testing.T) {
	table := engineSchemaTable(t, "fused_workspaces")
	for _, expected := range []string{
		"connect_display_name text NOT NULL DEFAULT 'Fused'",
		"connect_logo_url text NOT NULL DEFAULT ''",
		"connect_primary_color text NOT NULL DEFAULT '#6941ff'",
		"connect_primary_color_customized boolean NOT NULL DEFAULT false",
		"connect_support_url text NOT NULL DEFAULT ''",
		"connect_privacy_url text NOT NULL DEFAULT ''",
	} {
		// Every clean-schema field must carry its intended safe default directly.
		if !strings.Contains(table, expected) {
			t.Fatalf("workspace schema missing connect branding field %q", expected)
		}
	}
	legacy := strings.Join(connectBrandingMigrationQueries(), "\n")
	// Version five has already been recordable and cannot be retroactively edited.
	if !strings.Contains(legacy, "connect_primary_color text NOT NULL DEFAULT '#18181b'") || strings.Contains(legacy, "#2563eb") {
		t.Fatalf("version-five branding migration was rewritten: %s", legacy)
	}
}

// TestConnectBrandVioletMigrationConvergesOnlyUntouchedBlueDefaults locks the
// additive migration that aligns existing workspaces without replacing choices.
func TestConnectBrandVioletMigrationConvergesOnlyUntouchedBlueDefaults(t *testing.T) {
	queries := connectBrandVioletMigrationQueries()
	// Two statements keep row convergence and the insertion default independently auditable.
	if len(queries) != 2 {
		t.Fatalf("brand-violet migration query count = %d, want 2", len(queries))
	}
	joined := strings.Join(queries, "\n")
	for _, expected := range []string{
		"connect_primary_color = '#6941ff'",
		"connect_primary_color_customized = false",
		"connect_primary_color = '#2563eb'",
		"SET DEFAULT '#6941ff'",
	} {
		// Every predicate protects the boundary between a generated default and customer input.
		if !strings.Contains(joined, expected) {
			t.Fatalf("brand-violet migration missing %q: %s", expected, joined)
		}
	}
}

// TestConnectBrandColorMigrationConvergesOnlyUntouchedLegacyDefaults locks the
// version-six predicate and ordering that protect previously edited branding.
func TestConnectBrandColorMigrationConvergesOnlyUntouchedLegacyDefaults(t *testing.T) {
	queries := connectBrandColorMigrationQueries()
	// The count catches accidental replacement or reordering through query splits.
	if len(queries) != 4 {
		t.Fatalf("brand-colour migration query count = %d, want 4", len(queries))
	}
	joined := strings.Join(queries, "\n")
	for _, expected := range []string{
		"connect_primary_color_customized boolean NOT NULL DEFAULT false",
		"connect_primary_color IS DISTINCT FROM '#18181b'",
		"audit.resource_id = workspace.id",
		"audit.action = 'control.http.put'",
		"audit.path = '/workspace/connect-branding'",
		"audit.permission = 'workspace.update'",
		"audit.outcome = 'succeeded'",
		`audit.metadata @> '{"primary_color_changed": true}'::jsonb`,
		"connect_primary_color_customized = false",
		"connect_primary_color = '#18181b'",
		"connect_primary_color = '#2563eb'",
		"SET DEFAULT '#2563eb'",
	} {
		// Each predicate is part of the data-preservation contract for upgrades.
		if !strings.Contains(joined, expected) {
			t.Fatalf("brand-colour migration missing %q: %s", expected, joined)
		}
	}
	// Generic workspace timestamps cannot distinguish a colour choice from an
	// unrelated workspace mutation, so only the bounded audit fact is accepted.
	if strings.Contains(joined, "updated_at") {
		t.Fatalf("brand-colour migration uses ambiguous workspace timestamps: %s", joined)
	}
	// Protection must be established before any old default is replaced.
	if strings.Index(joined, "connect_primary_color_customized = true") > strings.Index(joined, "connect_primary_color = '#2563eb'") {
		t.Fatal("brand-colour migration must classify customized rows before converging defaults")
	}
}

// TestEngineSchemaDefinesImmutableUnifiedOperations verifies the fresh schema
// and unversioned convergence path both enforce the same final v2 shape.
func TestEngineSchemaDefinesImmutableUnifiedOperations(t *testing.T) {
	assertUnifiedFreshSchema(t, strings.Join(engineSchemaQueries(), "\n"))
	assertUnifiedConvergenceSchema(t, strings.Join(unifiedSchemaConvergenceQueries(), "\n"))
	assertUnifiedMigrationAbsent(t, strings.Join(engineMigrationQueries(), "\n"))
}

// assertUnifiedFreshSchema checks clean databases receive the exact final-v2 columns and checks.
func assertUnifiedFreshSchema(t *testing.T, schema string) {
	t.Helper()
	for _, expected := range []string{
		"unified_definition_schema_version integer NOT NULL DEFAULT 2",
		"unified_definitions    jsonb NOT NULL DEFAULT '[]'::jsonb",
		"CONSTRAINT chk_fused_apps_unified_definition_shape",
		"CONSTRAINT chk_fused_apps_unified_hashes",
	} {
		if !strings.Contains(schema, expected) {
			t.Fatalf("Unified app schema missing %q", expected)
		}
	}
}

// assertUnifiedConvergenceSchema checks live reconciliation adds only the canonical v2 shape.
func assertUnifiedConvergenceSchema(t *testing.T, convergence string) {
	t.Helper()
	for _, expected := range []string{
		"ADD COLUMN IF NOT EXISTS unified_definition_schema_version integer NOT NULL DEFAULT 2",
		"ALTER COLUMN unified_definition_schema_version SET DEFAULT 2",
		"ALTER COLUMN unified_definition_schema_version SET NOT NULL",
		"ADD CONSTRAINT chk_fused_apps_unified_definition_shape",
		"ADD CONSTRAINT chk_fused_apps_unified_hashes",
		"IF NOT EXISTS",
		"reset the Engine database before enabling Unified Operations",
	} {
		if !strings.Contains(convergence, expected) {
			t.Fatalf("Unified schema convergence missing %q", expected)
		}
	}
	for _, forbidden := range []string{"UPDATE fused_apps", "DROP CONSTRAINT"} {
		if strings.Contains(convergence, forbidden) {
			t.Fatalf("Unified schema convergence contains draft promotion %q", forbidden)
		}
	}
	// One conditional add per constraint prevents repeated startup validation.
	if strings.Count(convergence, "ADD CONSTRAINT chk_fused_apps_unified_definition_shape") != 1 ||
		strings.Count(convergence, "ADD CONSTRAINT chk_fused_apps_unified_hashes") != 1 {
		t.Fatal("Unified convergence must add each absent constraint exactly once")
	}
}

// assertUnifiedMigrationAbsent keeps canonical reconciliation outside the immutable ledger.
func assertUnifiedMigrationAbsent(t *testing.T, migrations string) {
	t.Helper()
	for _, forbidden := range []string{
		"unified_definition_schema_version",
		"unified_definitions",
		"unified_definition_hash",
		"unified_codegen_descriptor_hash",
		"chk_fused_apps_unified",
	} {
		if strings.Contains(migrations, forbidden) {
			t.Fatalf("Unified clean schema must not have migration fragment %q", forbidden)
		}
	}
}

func assertMigrationIdentity(t *testing.T, migration engineMigration, version int64, name string) {
	t.Helper()
	if migration.Version != version || migration.Name != name {
		t.Fatalf("migration identity = (%d, %q), want (%d, %q)", migration.Version, migration.Name, version, name)
	}
}

func TestEngineSchemaDefinesAppTokenPolicy(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"allow_all       boolean NOT NULL DEFAULT true",
		"allowed_operations text[] NOT NULL DEFAULT '{}'",
		"expires_at timestamptz",
		"CONSTRAINT chk_fused_app_tokens_allow",
		"NOT ('*' = ANY(allowed_operations))",
	} {
		if !strings.Contains(schema, expected) {
			t.Fatalf("app token schema missing %q", expected)
		}
	}

	migration := strings.Join(appTokenPolicyMigrationQueries(), "\n")
	for _, expected := range []string{
		"ADD COLUMN IF NOT EXISTS allow_all",
		"ADD COLUMN IF NOT EXISTS allowed_operations",
		"ADD COLUMN IF NOT EXISTS expires_at",
		"ADD CONSTRAINT chk_fused_app_tokens_allow",
	} {
		if !strings.Contains(migration, expected) {
			t.Fatalf("app token policy migration missing %q", expected)
		}
	}
}

func TestEngineSchemaDefinesCurrentConfigColumnsDirectly(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"desired_state     jsonb NOT NULL DEFAULT '{}'::jsonb",
		"managed_resources jsonb NOT NULL DEFAULT '{}'::jsonb",
		"latest_resource_id uuid",
		"actions          jsonb NOT NULL DEFAULT '[]'::jsonb",
		"resolved_payload jsonb NOT NULL DEFAULT '{}'::jsonb",
		"blockers         jsonb NOT NULL DEFAULT '[]'::jsonb",
		"warnings         jsonb NOT NULL DEFAULT '[]'::jsonb",
		"required_permissions jsonb NOT NULL",
		"revision         integer NOT NULL DEFAULT 1",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected current schema containing %q", expected)
		}
	}
}

func TestEngineSchemaDefinesExecutionTimeoutWithoutCompatibilityMigration(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(schema, "timeout_ms              integer") ||
		!strings.Contains(schema, "CHECK (timeout_ms IS NULL OR timeout_ms BETWEEN 1 AND 86400000)") {
		t.Fatal("clean Engine execution policy schema must define bounded timeout_ms")
	}
	if strings.Contains(schema, "fused_workspace_execution_policies ADD COLUMN IF NOT EXISTS timeout_ms") {
		t.Fatal("timeout_ms must not add a compatibility migration")
	}
}

func TestEngineMigrationPreservesOnlyCanonicalRateLimitJSON(t *testing.T) {
	migrations := strings.Join(engineMigrationQueries(), "\n")
	for _, expected := range []string{
		"UPDATE fused_workspace_execution_policies",
		"SET rate_limit = NULL",
		"rate_limit->>'version' IS DISTINCT FROM '3'",
	} {
		if !strings.Contains(migrations, expected) {
			t.Fatalf("rate-limit migration missing %q", expected)
		}
	}
	for _, preserved := range []string{"retry_config = NULL", "pagination = NULL", "base_url = NULL"} {
		if strings.Contains(migrations, preserved) {
			t.Fatalf("rate-limit migration must preserve unrelated field %q", preserved)
		}
	}
}

func TestEngineSchemaDefinesProviderRateLimitCoordinationAndActivity(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"CREATE TABLE IF NOT EXISTS fused_provider_rate_limit_states",
		"PRIMARY KEY (account_id, service_version_id, policy_name, scope_kind, scope_id)",
		"rate_limit_decision text",
		"rate_limit_unit_totals bigint[] NOT NULL DEFAULT '{}'",
	} {
		if !strings.Contains(schema, expected) {
			t.Fatalf("provider rate-limit schema missing %q", expected)
		}
	}
	migrations := strings.Join(engineMigrationQueries(), "\n")
	for _, expected := range []string{"rate_limit_policy_count", "rate_limit_header_outcome"} {
		if !strings.Contains(migrations, expected) {
			t.Fatalf("provider rate-limit migration missing %q", expected)
		}
	}
	if strings.Contains(migrations, "CREATE TABLE IF NOT EXISTS fused_provider_rate_limit_states") ||
		strings.Contains(migrations, "idx_fused_provider_rate_limit_states_updated") {
		t.Fatal("provider rate-limit base objects must not be recreated by startup migrations")
	}
	if !strings.Contains(migrations, "ADD COLUMN IF NOT EXISTS state_sequence") {
		t.Fatal("existing provider rate-limit projections must receive state_sequence")
	}
}

func TestEngineMigrationDoesNotRecreateBaseExecutionStartedIndex(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	migrations := strings.Join(engineMigrationQueries(), "\n")
	if !strings.Contains(schema, "idx_fused_engine_execution_events_started") {
		t.Fatal("fresh schema must create the execution started index")
	}
	if strings.Contains(migrations, "idx_fused_engine_execution_events_started ON") {
		t.Fatal("startup migrations must not recreate the base execution started index")
	}
}

func TestEngineSchemaDefinesOwnedWebhooksWithoutCompatibilityMigration(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(schema, "owning_config_key     text NOT NULL CHECK (owning_config_key <> '')") {
		t.Fatal("clean webhook schema must require an owning config key")
	}
	for _, required := range []string{
		"secret_bucket_id      uuid REFERENCES fused_buckets(id) ON DELETE RESTRICT",
		"CHECK ((secret_ref = '' AND secret_bucket_id IS NULL)",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("clean webhook schema must contain immutable secret binding %q", required)
		}
	}
	migrations := strings.Join(engineMigrationQueries(), "\n")
	for _, legacy := range []string{"fused_workspace_webhooks ADD COLUMN", "fused_config_states_config_type_check", "fused_config_plans_config_type_check"} {
		if strings.Contains(migrations, legacy) {
			t.Fatalf("webhook clean-cutover contract retained compatibility migration %q", legacy)
		}
	}
}

func TestEngineSchemaEnforcesImmutableConfigIdentity(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"fused_reject_config_identity_change",
		"OLD.config_type IS DISTINCT FROM NEW.config_type",
		"OLD.owner_subject_id IS DISTINCT FROM NEW.owner_subject_id",
		"OLD.owner_team_id IS DISTINCT FROM NEW.owner_team_id",
		"trg_fused_config_identity_immutable",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected immutable config identity schema containing %q", expected)
		}
	}
}

func TestEngineSchemaRequiresExactlyOneAppFamilyOwner(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"owner_subject_id    uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT",
		"(owner_subject_id IS NOT NULL)::int + (owner_team_id IS NOT NULL)::int = 1",
		"idx_fused_app_families_account_kind",
		"idx_fused_config_plans_subject_owner_status",
	} {
		if !strings.Contains(schema, expected) {
			t.Fatalf("clean schema must contain subject-or-team ownership rule %q", expected)
		}
	}
}

func TestEngineSchemaUsesKindScopedAppFamilyIdentity(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(schema, "UNIQUE (account_id, kind, canonical_name)") {
		t.Fatal("clean schema must bound SDK and MCP families independently by canonical name")
	}
	if !strings.Contains(schema, "UNIQUE (app_family_id, version)") {
		t.Fatal("clean schema must bind each semantic version within its app family")
	}
}

func TestEngineFreshSchemaDoesNotCreateLegacyArtifactPersistence(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	for _, table := range []string{
		"fused_artifact_scopes", "fused_artifact_tokens",
		"fused_artifact_buckets", "fused_artifact_snapshots",
		"fused_app_family_migrations",
	} {
		if strings.Contains(schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Fatalf("fresh schema must not create legacy table %s", table)
		}
	}
	for _, required := range []string{"fused_app_families", "fused_apps", "fused_app_tokens", "fused_app_family_buckets"} {
		if !strings.Contains(schema, "CREATE TABLE IF NOT EXISTS "+required) {
			t.Fatalf("fresh schema missing app persistence table %s", required)
		}
	}
}

func TestEngineRuntimeTablesUseExactAppIdentity(t *testing.T) {
	schemaQueries := engineSchemaQueries()
	migrationQueries := engineMigrationQueries()
	freshSchema := strings.Join(schemaQueries, "\n")
	assertSchemaContainsAll(t, freshSchema, "fresh runtime schema missing %q", []string{
		"CREATE TABLE IF NOT EXISTS fused_mcp_sessions",
		"app_id uuid",
		"CREATE TABLE IF NOT EXISTS fused_engine_idempotency_keys",
		"UNIQUE(app_id, idempotency_key_hash)",
		"response_media_family text NOT NULL DEFAULT 'unknown'",
	})
	if count := strings.Count(freshSchema, "CONSTRAINT chk_fused_engine_idempotency_response_media_family"); count != 1 {
		t.Fatalf("fresh runtime schema response media constraint count=%d, want 1", count)
	}
	assertSchemaContainsAll(t, strings.Join(migrationQueries, "\n"), "runtime identity migration missing %q", []string{
		"UPDATE fused_mcp_sessions SET app_id = artifact_id",
		"UPDATE fused_engine_idempotency_keys SET app_id = artifact_id",
		"fused_mcp_sessions DROP COLUMN IF EXISTS artifact_id",
		"fused_engine_idempotency_keys DROP COLUMN IF EXISTS artifact_id",
		"ADD COLUMN IF NOT EXISTS response_media_family text",
		"DELETE FROM fused_engine_idempotency_keys WHERE response_media_family IS NULL OR response_media_family = 'unknown'",
		"ALTER COLUMN response_media_family SET NOT NULL",
	})
	assertAppIdentityIndexesRunAfterLegacyColumns(t, schemaQueries, migrationQueries)
	assertSchemaOrder(t, migrationQueries, "fused_mcp_sessions ADD COLUMN IF NOT EXISTS app_id uuid", "idx_fused_mcp_sessions_app_started")
}

func assertAppIdentityIndexesRunAfterLegacyColumns(t *testing.T, schemaQueries, migrationQueries []string) {
	t.Helper()
	for _, index := range []string{
		"idx_fused_mcp_sessions_app_started",
		"idx_fused_engine_execution_events_family_started",
		"idx_fused_engine_execution_events_app_started",
		"idx_fused_engine_execution_events_endpoint",
	} {
		if indexOfSchemaFragment(schemaQueries, index) >= 0 {
			t.Fatalf("app identity index %q must run after legacy column migration", index)
		}
		if indexOfSchemaFragment(migrationQueries, index) < 0 {
			t.Fatalf("app identity migration missing index %q", index)
		}
	}
}

func TestEngineSchemaSupportsWorkspaceResourcePrincipalsWithoutLegacyMigration(t *testing.T) {
	schema := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(schema, "subject_type IN ('subject', 'team', 'workspace')") {
		t.Fatal("clean schema must allow workspace-wide resource bindings")
	}
	if strings.Contains(strings.Join(engineMigrationQueries(), "\n"), "chk_fused_role_bindings_subject_type") {
		t.Fatal("workspace principals belong in the clean schema, not a legacy migration")
	}
}

func TestEngineSchemaAllowsCanonicalAuditOutcomes(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(joined, "'attempted', 'allowed', 'denied', 'succeeded', 'failed', 'rolled_back', 'cancelled'") {
		t.Fatal("current audit schema must distinguish attempts, authorization, mutation results, rollback, and cancellation")
	}
	if strings.Contains(strings.Join(engineMigrationQueries(), "\n"), "rolled_back") {
		t.Fatal("canonical audit outcomes belong in the clean schema, not a legacy migration")
	}
}

func TestEngineSchemaDoesNotBackfillRequiredPermissions(t *testing.T) {
	migrations := strings.Join(engineMigrationQueries(), "\n")
	if strings.Contains(migrations, "required_permissions") {
		t.Fatal("required_permissions belongs in the clean schema, not legacy migrations")
	}
}

func TestEngineSchemaDefinesBucketAttachedConnectAuth(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_connect_configs",
		"CREATE TABLE IF NOT EXISTS fused_auth_connections",
		"CREATE TABLE IF NOT EXISTS fused_connect_sessions",
		"CONSTRAINT uq_fused_auth_connections UNIQUE (bucket_id, service_id, end_user_ref, auth_name)",
		"created_by_app_id       uuid",
		"identity_claims    jsonb NOT NULL DEFAULT '{}'::jsonb",
		"encrypted_dek      text NOT NULL DEFAULT ''",
		"'reconnect_required'",
		"last_failure_code  text NOT NULL DEFAULT ''",
		"last_failure_at    timestamptz",
		"last_failure_trace_id text NOT NULL DEFAULT ''",
		"service_version_id uuid",
		"last_refresh_attempt_at timestamptz",
		"last_refreshed_at  timestamptz",
		"refresh_retry_not_before timestamptz",
		"refresh_lease_token uuid",
		"refresh_lease_expires_at timestamptz",
		"chk_fused_auth_connections_refresh_lease",
		"CREATE INDEX IF NOT EXISTS idx_fused_auth_connections_refresh",
		"CREATE INDEX IF NOT EXISTS idx_fused_connect_sessions_expires",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected bucket-attached Connect auth schema containing %q", expected)
		}
	}
}

// TestManagedOAuthRefreshMigrationPinsOnlyUnambiguousLegacyRows locks the
// additive v8 schema and its fail-closed legacy service-version backfill.
func TestManagedOAuthRefreshMigrationPinsOnlyUnambiguousLegacyRows(t *testing.T) {
	joined := strings.Join(managedOAuthRefreshMigrationQueries(), "\n")
	for _, expected := range []string{
		"ADD COLUMN IF NOT EXISTS service_version_id uuid",
		"HAVING COUNT(*) = 1",
		"status <> 'deprecated'",
		"last_refresh_attempt_at timestamptz",
		"refresh_lease_token uuid",
		"refresh_retry_not_before timestamptz",
		"CHECK (service_version_id IS NOT NULL) NOT VALID",
		"service_version_id IS NOT NULL",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("managed OAuth refresh migration missing %q", expected)
		}
	}
	// Why: choosing MIN or latest without the count guard would silently bind a
	// credential to one of several different provider token contracts.
	if strings.Contains(joined, "ORDER BY enabled_at DESC") {
		t.Fatal("legacy refresh backfill must not choose a latest version")
	}
}

func TestEngineSchemaDefinesRuntimeReportingTables(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_runtime_entitlements",
		"entitlement_revision text NOT NULL DEFAULT ''",
		"heartbeat_interval_seconds integer NOT NULL",
		"CREATE TABLE IF NOT EXISTS fused_engine_usage_counter_reports",
		"uq_fused_engine_usage_pending_counter",
		"WHERE flushed_at IS NULL",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected runtime reporting schema containing %q", expected)
		}
	}
}

// TestEngineMigrationsExpandRefreshStateConstraint protects the
// reconnect_required refresh_state value: chk_fused_auth_connections_refresh_state's
// 'reconnect_required' value is (re)established via an unconditional DROP
// CONSTRAINT IF EXISTS + ADD CONSTRAINT in the migration list, which converges
// to the same end state whether the constraint was previously missing, stale,
// or already correct.
func TestEngineMigrationsExpandRefreshStateConstraint(t *testing.T) {
	schemaJoined := strings.Join(engineSchemaQueries(), "\n")
	for _, expected := range []string{
		"last_failure_code  text NOT NULL DEFAULT ''",
		"last_failure_at    timestamptz",
		"last_failure_trace_id text NOT NULL DEFAULT ''",
	} {
		if !strings.Contains(schemaJoined, expected) {
			t.Fatalf("expected fused_auth_connections CREATE TABLE containing %q", expected)
		}
	}

	migrationJoined := strings.Join(engineMigrationQueries(), "\n")
	for _, expected := range []string{
		"DROP CONSTRAINT IF EXISTS chk_fused_auth_connections_refresh_state",
		"ADD CONSTRAINT chk_fused_auth_connections_refresh_state",
		"'reconnect_required'",
	} {
		if !strings.Contains(migrationJoined, expected) {
			t.Fatalf("expected reconnect-state migration containing %q", expected)
		}
	}
}

func TestEngineSchemaMigratesWebhookAnalyticsToCanonicalEvents(t *testing.T) {
	joined := strings.Join(engineMigrationQueries(), "\n")
	for _, expected := range []string{
		"ADD COLUMN IF NOT EXISTS direction",
		"ADD COLUMN IF NOT EXISTS webhook_id",
		"idx_fused_engine_execution_events_webhook_started",
		"DROP TABLE IF EXISTS fused_webhook_events",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected canonical webhook migration containing %q", expected)
		}
	}
}

func TestEngineSchemaDefinesVersionAwareAppExecutionIdentity(t *testing.T) {
	eventTable := schemaQueryContaining(engineSchemaQueries(), "CREATE TABLE IF NOT EXISTS fused_engine_execution_events")
	if eventTable == "" {
		t.Fatal("canonical execution-event table is missing")
	}
	assertSchemaContainsAll(t, eventTable, "expected execution-event schema containing %q", []string{
		"app_family_id uuid",
		"app_id uuid",
		"app_version text",
		"provider_protocol text",
		"chk_fused_execution_app_identity",
	})
	if strings.Contains(eventTable, "artifact_id") {
		t.Fatal("canonical execution-event table must not retain artifact_id")
	}

	migrationQueries := engineMigrationQueries()
	assertSchemaContainsAll(t, strings.Join(migrationQueries, "\n"), "expected execution-event migration containing %q", []string{
		"UPDATE fused_engine_execution_events event",
		"SELECT app_id, app_family_id, version FROM fused_apps",
		"SELECT app_id, app_family_id, version FROM fused_app_tombstones",
		"DROP COLUMN IF EXISTS artifact_id",
		"idx_fused_engine_execution_events_family_started",
		"idx_fused_engine_execution_events_app_started",
	})
	assertSchemaOrder(t, migrationQueries, "fused_engine_execution_events ADD COLUMN IF NOT EXISTS app_id uuid", "idx_fused_engine_execution_events_app_started")
	assertSchemaOrder(t, migrationQueries, "UPDATE fused_engine_execution_events event", "fused_engine_execution_events DROP COLUMN IF EXISTS artifact_id")
}

func schemaQueryContaining(queries []string, fragment string) string {
	for _, query := range queries {
		if strings.Contains(query, fragment) {
			return query
		}
	}
	return ""
}

func assertSchemaContainsAll(t *testing.T, schema, failureFormat string, expectedFragments []string) {
	t.Helper()
	for _, expected := range expectedFragments {
		if !strings.Contains(schema, expected) {
			t.Fatalf(failureFormat, expected)
		}
	}
}

func indexOfSchemaFragment(queries []string, fragment string) int {
	for i, query := range queries {
		if strings.Contains(query, fragment) {
			return i
		}
	}
	return -1
}

// TestEngineSchemaDefinesWorkspaceScopedConnectionProfiles protects the
// workspace + service + service_version + auth_type profile identity
// (plans/workspace_connection_profile_scope_plan.md) -- these tables replaced
// the old bucket-owned fused_bucket_profile_attachments/fused_bucket_bindings.
func TestEngineSchemaDefinesWorkspaceScopedConnectionProfiles(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_workspace_connection_profiles",
		"layer                   text NOT NULL",
		"CONSTRAINT uq_fused_workspace_connection_profile\n\t\t\t\tUNIQUE (service_id, service_version_id, auth_type, layer)",
		"CHECK (layer IN ('baseline', 'override'))",
		"CHECK (layer <> 'baseline' OR provenance IN ('provider', 'fused'))",
		"CHECK (layer <> 'baseline' OR registry_profile_id IS NOT NULL)",
		"CHECK (layer <> 'override' OR (provenance = 'workspace' AND registry_profile_id IS NULL))",
		"CREATE TABLE IF NOT EXISTS fused_workspace_connection_bindings",
		"profile_id              uuid NOT NULL REFERENCES fused_workspace_connection_profiles(id) ON DELETE CASCADE",
		"CREATE INDEX IF NOT EXISTS idx_fused_workspace_connection_profiles_lookup",
		"CREATE INDEX IF NOT EXISTS idx_fused_workspace_connection_bindings_operations",
		"USING GIN(operation_ids)",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected workspace-scoped connection profile schema containing %q", expected)
		}
	}
	forbidden := []string{"bucket_id", "locally_overridden"}
	bindingsStart := strings.Index(joined, "CREATE TABLE IF NOT EXISTS fused_workspace_connection_bindings")
	if bindingsStart < 0 {
		t.Fatal("fused_workspace_connection_bindings table definition not found")
	}
	bindingsSection := joined[bindingsStart:]
	bindingsEnd := strings.Index(bindingsSection, ");")
	if bindingsEnd < 0 {
		t.Fatal("fused_workspace_connection_bindings table definition has no closing statement")
	}
	for _, fragment := range forbidden {
		if strings.Contains(bindingsSection[:bindingsEnd], fragment) {
			t.Fatalf("fused_workspace_connection_bindings must not contain %q", fragment)
		}
	}
}

func TestEngineSchemaDefinesActivatedContractSnapshots(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_service_contract_snapshots",
		"service_version_id uuid NOT NULL UNIQUE",
		"contract_version   integer NOT NULL",
		"required_capabilities text[] NOT NULL",
		"contract_hash      text NOT NULL",
		"contract_status    text NOT NULL DEFAULT 'active'",
		"service_metadata   jsonb NOT NULL",
		"CREATE TABLE IF NOT EXISTS fused_service_contract_endpoints",
		"operation_json  jsonb NOT NULL",
		"UNIQUE (snapshot_id, name)",
		"CREATE TABLE IF NOT EXISTS fused_service_contract_webhooks",
		"webhook_json jsonb NOT NULL",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected activated contract snapshot schema containing %q", expected)
		}
	}
	forbidden := []string{"raw_source", "source_content", "openapi_spec"}
	snapshotStart := strings.Index(joined, "CREATE TABLE IF NOT EXISTS fused_service_contract_snapshots")
	if snapshotStart < 0 {
		t.Fatal("fused_service_contract_snapshots table definition not found")
	}
	snapshotSection := joined[snapshotStart:]
	snapshotEnd := strings.Index(snapshotSection, ");")
	if snapshotEnd < 0 {
		t.Fatal("fused_service_contract_snapshots table definition has no closing statement")
	}
	for _, fragment := range forbidden {
		if strings.Contains(snapshotSection[:snapshotEnd], fragment) {
			t.Fatalf("contract snapshots must not contain raw source fragment %q", fragment)
		}
	}
}

func TestContractEnvelopeMigrationFailsClosedForExistingSnapshots(t *testing.T) {
	migration := strings.Join(contractEnvelopeMigrationQueries(), "\n")
	required := []string{
		"ADD COLUMN IF NOT EXISTS contract_version integer",
		"ADD COLUMN IF NOT EXISTS required_capabilities text[]",
		"WHERE contract_version IS NULL OR required_capabilities IS NULL",
		"ALTER COLUMN contract_version SET NOT NULL",
		"ALTER COLUMN required_capabilities SET NOT NULL",
	}
	for _, fragment := range required {
		if !strings.Contains(migration, fragment) {
			t.Fatalf("contract envelope migration missing %q", fragment)
		}
	}
}

// TestEngineSchemaRestrictsArtifactsToOneBucket protects the runtime contract:
// an SDK or MCP scope can contain many services, but resolves all of them from
// one bucket selected when the artifact is provisioned.
func TestEngineSchemaRestrictsAppFamiliesToOneBucket(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(joined, "app_family_id uuid PRIMARY KEY") {
		t.Fatal("fused_app_family_buckets must allow exactly one bucket row per app family")
	}
}

// TestEngineMigrationsDropOldBucketOwnedProfileTables protects the one-way
// removal required by the plan: the product is not live, so the old
// bucket-owned tables are dropped outright rather than dual-written.
func TestEngineMigrationsDropOldBucketOwnedProfileTables(t *testing.T) {
	joined := strings.Join(engineMigrationQueries(), "\n")
	required := []string{
		"DROP TABLE IF EXISTS fused_bucket_bindings;",
		"DROP TABLE IF EXISTS fused_bucket_profile_attachments;",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected migration containing %q", expected)
		}
	}
	// Bindings must be dropped before attachments since bindings FK to attachments.
	if strings.Index(joined, "DROP TABLE IF EXISTS fused_bucket_bindings;") > strings.Index(joined, "DROP TABLE IF EXISTS fused_bucket_profile_attachments;") {
		t.Fatalf("expected fused_bucket_bindings to be dropped before fused_bucket_profile_attachments")
	}
}

// TestEngineSchemaContainsNoLegacyPersistence admits only the explicit Unified
// convergence statements at the otherwise clean canonical schema boundary.
func TestEngineSchemaContainsNoLegacyPersistence(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	forbidden := []string{
		"fused_accounts",
		"fused_api_keys",
		"fused_workspace_configs",
		"DROP TABLE",
		"DROP INDEX",
		"DELETE FROM",
		"INSERT INTO fused_activation_versions",
		"ROW_NUMBER() OVER",
	}
	for _, fragment := range forbidden {
		if strings.Contains(joined, fragment) {
			t.Fatalf("Engine schema must not contain legacy persistence fragment %q", fragment)
		}
	}
	allowedAlter := make(map[string]struct{}, len(unifiedSchemaConvergenceQueries()))
	for _, query := range unifiedSchemaConvergenceQueries() {
		allowedAlter[query] = struct{}{}
	}
	for _, query := range engineSchemaQueries() {
		if !strings.HasPrefix(strings.TrimSpace(query), "ALTER TABLE") {
			continue
		}
		// Final-v2 Unified convergence is the only unversioned live-table
		// adjustment admitted at the clean schema boundary.
		if _, allowed := allowedAlter[query]; !allowed {
			t.Fatalf("Engine schema contains unapproved convergence statement %q", query)
		}
	}
}

// TestEngineSchemaAllowsNonBreakingNotificationSeverity protects the
// service changelog notification system (plans/plan-service-changelog.md): the
// severity CHECK constraint originally only allowed 'breaking', which would
// silently reject every non-breaking changelog notification insert. Both the
// fresh-database CREATE TABLE and the existing-database migration must allow
// 'non-breaking'.
func TestEngineSchemaAllowsNonBreakingNotificationSeverity(t *testing.T) {
	schemaJoined := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(schemaJoined, "CHECK (severity IN ('breaking', 'non-breaking'))") {
		t.Fatalf("expected fused_workspace_notifications CREATE TABLE to allow non-breaking severity")
	}

	migrationJoined := strings.Join(engineMigrationQueries(), "\n")
	required := []string{
		"DROP CONSTRAINT IF EXISTS fused_workspace_notifications_severity_check",
		"ADD CONSTRAINT fused_workspace_notifications_severity_check CHECK (severity IN ('breaking', 'non-breaking'))",
	}
	for _, expected := range required {
		if !strings.Contains(migrationJoined, expected) {
			t.Fatalf("expected migration widening the severity constraint containing %q", expected)
		}
	}
	// The DROP must precede the ADD, otherwise the ADD would apply against
	// whatever constraint (if any) was already there under a different name.
	dropIdx := strings.Index(migrationJoined, "DROP CONSTRAINT IF EXISTS fused_workspace_notifications_severity_check")
	addIdx := strings.Index(migrationJoined, "ADD CONSTRAINT fused_workspace_notifications_severity_check")
	if dropIdx < 0 || addIdx < 0 || dropIdx > addIdx {
		t.Fatalf("expected DROP CONSTRAINT before ADD CONSTRAINT for fused_workspace_notifications_severity_check")
	}
}

func TestEngineSchemaEnforcesSingletonWorkspaceWithoutAccountOwnership(t *testing.T) {
	joined := strings.Join(append(engineSchemaQueries(), engineMigrationQueries()...), "\n")
	required := []string{
		"account_id uuid NOT NULL",
		"name text NOT NULL",
		"singleton_key smallint NOT NULL DEFAULT 1 UNIQUE",
		"CHECK (singleton_key = 1)",
		"service_version_id uuid NOT NULL",
		"session_id text",
		"ended_at timestamp with time zone",
		"CREATE TABLE IF NOT EXISTS fused_engine_execution_events",
		"trace_id text",
		"provider_latency_ms bigint",
		"request_body_hash text",
		"version text",
		"config_key             text NOT NULL",
		"idx_fused_apps_family_status",
		"config_type IN ('workspace', 'sdk', 'mcp', 'webhook')",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected Engine schema query containing %q", expected)
		}
	}

}
