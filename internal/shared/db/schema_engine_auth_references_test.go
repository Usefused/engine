package db

import (
	"strings"
	"testing"
)

// TestEngineSchemaDefinesWorkspaceAuthReferenceOwnership locks cleanup and
// dependency behavior into the clean Engine schema.
func TestEngineSchemaDefinesWorkspaceAuthReferenceOwnership(t *testing.T) {
	table := engineSchemaTable(t, "fused_workspace_auth_references")
	for _, expected := range []string{
		"bucket_id         uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE",
		"CONSTRAINT fk_fused_workspace_auth_reference_target FOREIGN KEY (target_service_id)",
		"REFERENCES fused_workspace_services(service_id) ON DELETE CASCADE",
		"CONSTRAINT fk_fused_workspace_auth_reference_source FOREIGN KEY (source_service_id)",
		"REFERENCES fused_workspace_services(service_id) ON DELETE NO ACTION",
		"CONSTRAINT uq_fused_workspace_auth_reference_target UNIQUE (bucket_id, target_service_id, target_auth_name)",
		"CONSTRAINT chk_fused_workspace_auth_reference_not_self CHECK",
		"target_service_id <> source_service_id OR target_auth_name <> source_auth_name",
	} {
		// Targets own their binding, while sources remain protected until dependents move.
		if !strings.Contains(table, expected) {
			t.Fatalf("workspace auth reference schema missing %q: %s", expected, table)
		}
	}
	joined := strings.Join(engineSchemaQueries(), "\n")
	// Source dependency checks need one index across the exact referenced family.
	if !strings.Contains(joined, "ON fused_workspace_auth_references(bucket_id, source_service_id, source_auth_name)") {
		t.Fatal("workspace auth reference schema is missing the source-family index")
	}
	migrations := engineMigrations()
	latest := migrations[len(migrations)-1]
	// Existing Engines need the same NO ACTION boundary as fresh schemas.
	if latest.Version != workspaceAuthReferenceMigrationVersion || latest.Name != workspaceAuthReferenceMigrationName {
		t.Fatalf("latest workspace auth reference migration = %+v", latest)
	}
	migrationSQL := strings.Join(workspaceAuthReferenceMigrationQueries(), "\n")
	// Constraint replacement must keep its stable name so store conflicts remain typed.
	if !strings.Contains(migrationSQL, "DROP CONSTRAINT IF EXISTS fk_fused_workspace_auth_reference_source") || !strings.Contains(migrationSQL, "ON DELETE NO ACTION") {
		t.Fatalf("workspace auth reference migration is incomplete: %s", migrationSQL)
	}
}
