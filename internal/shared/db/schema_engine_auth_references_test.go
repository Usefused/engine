package db

import (
	"strings"
	"testing"
)

// TestEngineSchemaRetiresWorkspaceAuthReferenceOwnership preserves migration history while removing its runtime table.
func TestEngineSchemaRetiresWorkspaceAuthReferenceOwnership(t *testing.T) {
	migrations := engineMigrations()
	// Existing Engines retain the shipped v15 ledger entry before the forward cleanup.
	assertMigrationIdentity(t, migrations[14], workspaceAuthReferenceMigrationVersion, workspaceAuthReferenceMigrationName)
	assertMigrationIdentity(t, migrations[16], appCredentialSourceMigrationVersion, appCredentialSourceMigrationName)
	migrationSQL := strings.Join(workspaceAuthReferenceMigrationQueries(), "\n")
	// Historical SQL remains immutable even though no live runtime code uses its table.
	if !strings.Contains(migrationSQL, "DROP CONSTRAINT IF EXISTS fk_fused_workspace_auth_reference_source") || !strings.Contains(migrationSQL, "ON DELETE NO ACTION") {
		t.Fatalf("workspace auth reference migration is incomplete: %s", migrationSQL)
	}
	cleanupSQL := strings.Join(appCredentialSourceMigrationQueries(), "\n")
	// Source identities move to sessions/connections before the incorrect global table is dropped.
	if !strings.Contains(cleanupSQL, "credential_source_service_id = reference.source_service_id") || !strings.Contains(cleanupSQL, "DROP TABLE IF EXISTS fused_workspace_auth_references") {
		t.Fatalf("workspace auth reference cleanup is incomplete: %s", cleanupSQL)
	}
}
