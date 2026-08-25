package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestDecodeMCPUnifiedDescriptorProjection verifies hash admission applies to
// the complete descriptor rather than its token-filtered projection.
func TestDecodeMCPUnifiedDescriptorProjection(t *testing.T) {
	complete := []byte(`{"schema_version":3,"operations":[{"name":"all","input_schema":{},"targets":[]}]}`)
	visible := []byte(`{"schema_version":3,"operations":[]}`)
	digest, err := canonicaljson.HexSHA256(complete)
	if err != nil {
		t.Fatalf("hash descriptor fixture: %v", err)
	}
	descriptor, err := decodeMCPUnifiedDescriptorProjection(complete, visible, "sha256:"+digest)
	if err != nil || descriptor != nil {
		t.Fatalf("filtered descriptor = (%#v, %v), want nil visible catalogue", descriptor, err)
	}
	// A filtered value must never validate against its own hash because app
	// identity pins the complete applied-plan descriptor.
	if _, err := decodeMCPUnifiedDescriptorProjection(complete, complete, EmptyUnifiedSetHash); err == nil {
		t.Fatal("descriptor hash mismatch error = nil")
	}
}

// TestGetMCPUnifiedOperationDescriptorsFiltersStrictPolicyInSQL exercises the
// exact applied-plan recovery and whole-operation token predicate in PostgreSQL.
func TestGetMCPUnifiedOperationDescriptorsFiltersStrictPolicyInSQL(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Package tests remain self-contained when PostgreSQL integration is not configured.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)
	accountID, familyID, appID, planID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	teamID := seedAppOwnerTeam(t, ctx, pool)
	// Cleanup targets only generated identities so a shared developer database
	// retains unrelated application state.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE id=$1`, planID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_id=$1`, appID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, familyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_teams WHERE id=$1`, teamID)
	})

	descriptors := mcpUnifiedDescriptorFixture()
	encoded, err := json.Marshal(descriptors)
	if err != nil {
		t.Fatalf("encode descriptor fixture: %v", err)
	}
	digest, err := canonicaljson.HexSHA256(encoded)
	if err != nil {
		t.Fatalf("hash descriptor fixture: %v", err)
	}
	seedMCPUnifiedDescriptorFixture(t, ctx, repository, teamID, accountID, familyID, appID, planID, "sha256:"+digest, descriptors)

	visible, err := repository.GetMCPUnifiedOperationDescriptors(ctx, appID, false, []string{"getUser", "deleteUser"})
	if err != nil {
		t.Fatalf("GetMCPUnifiedOperationDescriptors() error = %v", err)
	}
	if visible == nil || len(visible.Operations) != 1 || visible.Operations[0].Name != "users.read" {
		t.Fatalf("strict descriptor projection = %#v, want only fully allowed graph", visible)
	}
	all, err := repository.GetMCPUnifiedOperationDescriptors(ctx, appID, true, nil)
	if err != nil || all == nil || len(all.Operations) != 2 {
		t.Fatalf("unrestricted descriptor projection = (%#v, %v), want both operations", all, err)
	}
}

// mcpUnifiedDescriptorFixture builds two public graphs, one with compensation,
// so SQL filtering proves both forward and rollback policy membership.
func mcpUnifiedDescriptorFixture() *models.SDKUnifiedOperationDescriptors {
	return &models.SDKUnifiedOperationDescriptors{SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion, Operations: []models.SDKUnifiedOperationDescriptor{
		{Name: "users.read", InputSchema: json.RawMessage(`{"type":"object"}`), Targets: []models.SDKUnifiedTargetDescriptor{{
			PublicTarget: "read", OperationID: "getUser", ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointID: uuid.New(),
			Rollback: &models.SDKUnifiedRollbackDescriptor{OperationID: "deleteUser", ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointID: uuid.New()},
		}}},
		{Name: "users.write", InputSchema: json.RawMessage(`{"type":"object"}`), Targets: []models.SDKUnifiedTargetDescriptor{{
			PublicTarget: "write", OperationID: "createUser", ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointID: uuid.New(),
		}}},
	}}
}

// seedMCPUnifiedDescriptorFixture persists one runnable MCP version and the
// exact applied plan that owns its public descriptor.
func seedMCPUnifiedDescriptorFixture(t *testing.T, ctx context.Context, repository *postgresStore, teamID, accountID, familyID, appID, planID uuid.UUID, descriptorHash string, descriptors *models.SDKUnifiedOperationDescriptors) {
	t.Helper()
	family, _, err := repository.CreateOrGetAppFamily(ctx, AppFamily{
		AppFamilyID: familyID, AccountID: accountID, Kind: AppKindMCP,
		CanonicalName: "users-mcp", DisplayName: "Users MCP", OwnerTeamID: teamID,
	})
	if err != nil {
		t.Fatalf("create MCP family: %v", err)
	}
	selections, _ := json.Marshal([]models.SDKSelection{{SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true}})
	sourceHash := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	configKey := "mcp:Users MCP:1.0.0"
	_, _, err = repository.PublishAppVersion(ctx, App{
		AppID: appID, AppFamilyID: family.AppFamilyID, AccountID: accountID, Version: "1.0.0",
		ConfigKey: configKey, SourceHash: sourceHash, CapabilityHash: "capability",
		ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
		UnifiedDefinitionSchemaVersion: UnifiedDefinitionSchemaVersion,
		UnifiedDefinitions:             []byte("[]"), UnifiedDefinitionHash: EmptyUnifiedSetHash,
		UnifiedCodegenDescriptorHash: descriptorHash, Status: AppStatusActive, ExpectedFamilyKind: AppKindMCP,
	})
	if err != nil {
		t.Fatalf("publish MCP app: %v", err)
	}
	resolved, _ := json.Marshal(map[string]any{"unified_operations": descriptors})
	_, err = repository.db.Exec(ctx, `
		INSERT INTO fused_config_plans
			(id, config_key, config_type, owner_team_id, source_hash, status,
			 actions, desired_state, resolved_payload, blockers, warnings,
			 required_permissions, applied_at)
		VALUES ($1,$2,'mcp',$3,$4,'applied','[]','{}',$5,'[]','[]','[]',NOW())
	`, planID, configKey, teamID, sourceHash, resolved)
	if err != nil {
		t.Fatalf("seed applied MCP plan: %v", err)
	}
}
