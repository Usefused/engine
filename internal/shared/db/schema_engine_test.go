package db

import (
	"strings"
	"testing"
)

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
		"OLD.owner_team_id IS DISTINCT FROM NEW.owner_team_id",
		"trg_fused_config_identity_immutable",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected immutable config identity schema containing %q", expected)
		}
	}
}

func TestEngineSchemaAllowsAttemptedAuditOutcome(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(joined, "'attempted', 'allowed', 'denied', 'succeeded', 'failed'") {
		t.Fatal("current audit schema must distinguish attempted from allowed")
	}
	if strings.Contains(strings.Join(engineMigrationQueries(), "\n"), "attempted") {
		t.Fatal("attempted audit outcome belongs in the clean schema, not a legacy migration")
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
		"CONSTRAINT uq_fused_auth_connections UNIQUE (bucket_id, service_id, end_user_ref)",
		"created_by_artifact_id  uuid",
		"identity_claims    jsonb NOT NULL DEFAULT '{}'::jsonb",
		"encrypted_dek      text NOT NULL DEFAULT ''",
		"'reconnect_required'",
		"last_failure_code  text NOT NULL DEFAULT ''",
		"last_failure_at    timestamptz",
		"last_failure_trace_id text NOT NULL DEFAULT ''",
		"CREATE INDEX IF NOT EXISTS idx_fused_auth_connections_refresh",
		"CREATE INDEX IF NOT EXISTS idx_fused_connect_sessions_expires",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected bucket-attached Connect auth schema containing %q", expected)
		}
	}
}

func TestEngineSchemaDefinesRuntimeReportingTables(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	required := []string{
		"CREATE TABLE IF NOT EXISTS fused_runtime_entitlements",
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

func TestEngineSchemaCreatesWebhookEventsBeforeIndexes(t *testing.T) {
	queries := engineSchemaQueries()
	tableIndex := indexOfSchemaFragment(queries, "CREATE TABLE IF NOT EXISTS fused_webhook_events")
	if tableIndex < 0 {
		t.Fatal("fused_webhook_events table definition not found")
	}
	for _, fragment := range []string{
		"idx_fused_webhook_events_account_id",
		"idx_fused_webhook_events_service_id",
		"uq_fused_webhook_events_msg_id",
		"idx_fused_webhook_events_created_at",
	} {
		indexQuery := indexOfSchemaFragment(queries, fragment)
		if indexQuery < 0 {
			t.Fatalf("webhook event index %q not found", fragment)
		}
		if indexQuery < tableIndex {
			t.Fatalf("webhook event index %q appears before fused_webhook_events table", fragment)
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

// TestEngineSchemaRestrictsArtifactsToOneBucket protects the runtime contract:
// an SDK or MCP scope can contain many services, but resolves all of them from
// one bucket selected when the artifact is provisioned.
func TestEngineSchemaRestrictsArtifactsToOneBucket(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	if !strings.Contains(joined, "PRIMARY KEY (artifact_id)") {
		t.Fatal("fused_artifact_buckets must allow exactly one bucket row per artifact")
	}
	migrations := strings.Join(engineMigrationQueries(), "\n")
	if !strings.Contains(migrations, "CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_artifact_buckets_artifact_id ON fused_artifact_buckets(artifact_id)") {
		t.Fatal("existing Engine databases must receive the one-bucket artifact invariant")
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

func TestEngineSchemaContainsNoLegacyPersistence(t *testing.T) {
	joined := strings.Join(engineSchemaQueries(), "\n")
	forbidden := []string{
		"fused_accounts",
		"fused_api_keys",
		"fused_workspace_configs",
		"ALTER TABLE",
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
}

// TestEngineSchemaAllowsNonBreakingNotificationSeverity protects Phase 3 of
// the service changelog system (plans/plan-service-changelog.md): the
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
		"config_key text",
		"idx_fused_artifact_scopes_artifact_identity",
		"config_type IN ('workspace', 'sdk', 'mcp', 'webhook')",
	}
	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected Engine schema query containing %q", expected)
		}
	}

}
