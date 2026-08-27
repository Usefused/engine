package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestGetSDKPackageBuildRequestUsesExactAppliedPlan proves package recovery preserves its applied generation pin even after a runtime refresh.
func TestGetSDKPackageBuildRequestUsesExactAppliedPlan(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Never guess a developer's database for integration writes.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Schema initialization precedes all isolated fixture writes.
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	// Fixture cleanup must run before the pool closes so retained snapshot rows are actually removed.
	t.Cleanup(pool.Close)
	repository := NewPostgresStore(pool).(*postgresStore)
	accountID, familyID, appID, planID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	teamID := seedAppOwnerTeam(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE id=$1`, planID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_id=$1`, appID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, familyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_teams WHERE id=$1`, teamID)
	})

	family, _, err := repository.CreateOrGetAppFamily(ctx, AppFamily{
		AppFamilyID: familyID, AccountID: accountID, Kind: "sdk",
		CanonicalName: "jira-sdk", DisplayName: "Jira-SDK", TargetLanguage: "typescript", OwnerTeamID: teamID,
	})
	// Family ownership must exist before immutable version publication.
	if err != nil {
		t.Fatalf("create SDK family: %v", err)
	}
	selection := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointIDs: []uuid.UUID{uuid.New()}}
	selections, _ := json.Marshal([]models.SDKSelection{selection})
	sourceHash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	configKey := "sdk:Jira-SDK:2.0.0"
	// Deprecated versions remain downloadable and retain their original build inputs.
	if _, _, err := repository.PublishAppVersion(ctx, App{
		AppID: appID, AppFamilyID: family.AppFamilyID, AccountID: accountID,
		Version: "2.0.0", ConfigKey: configKey, SourceHash: sourceHash,
		CapabilityHash: "capability", ScopeSchemaVersion: models.AppScopeSchemaVersion,
		Selections: selections, GeneratorVersion: models.SDKGeneratorVersion, Status: "deprecated",
		ExpectedFamilyKind: AppKindSDK,
	}); err != nil {
		t.Fatalf("publish SDK app: %v", err)
	}
	binding := models.SDKContractBinding{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID, Version: "2026-01", Revision: 7, SourceHash: "contract", GenerationContractHash: "sha256:" + strings.Repeat("a", 64)}
	credentialSourceBinding := models.SDKContractBinding{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Version: "2026-02", Revision: 3, SourceHash: "source-contract", RuntimeContractHash: "sha256:" + strings.Repeat("c", 64)}
	resolved, _ := json.Marshal(map[string]any{
		"description": "Pinned Jira SDK", "default_engine_url": "https://tenant-exec.example.com:443",
		"contract_bindings":          []models.SDKContractBinding{binding},
		"credential_source_bindings": []models.SDKContractBinding{credentialSourceBinding},
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO fused_config_plans
			(id, config_key, config_type, owner_team_id, source_hash, status,
			 actions, desired_state, resolved_payload, blockers, warnings,
			 required_permissions, applied_at)
		VALUES ($1,$2,'sdk',$3,$4,'applied','[]','{}',$5,'[]','[]','[]',NOW())
	`, planID, configKey, teamID, sourceHash, resolved)
	// Only the exact applied plan may authorize reconstruction after cache eviction.
	if err != nil {
		t.Fatalf("seed applied SDK plan: %v", err)
	}

	request, err := repository.GetSDKPackageBuildRequest(ctx, accountID, appID)
	// A missing build definition must not be replaced by live Registry metadata.
	if err != nil {
		t.Fatalf("GetSDKPackageBuildRequest() error = %v", err)
	}
	assertSDKPackageBuildIdentity(t, request, appID, familyID, planID)
	assertSDKPackageBuildBinding(t, request, binding)
	snapshot := serviceContractBatchFixture(1)
	snapshot.ServiceID, snapshot.ServiceVersionID = selection.ServiceID, selection.ServiceVersionID
	snapshot.ServiceMetadata.ID, snapshot.ServiceMetadata.ServiceVersionID = selection.ServiceID, selection.ServiceVersionID
	snapshot.GenerationContractHash, snapshot.Revision = "sha256:"+strings.Repeat("b", 64), 99
	t.Cleanup(func() { deleteServiceContractFixture(t, pool, snapshot.ServiceVersionID) })
	// A later runtime refresh must not alter the package's applied generation contract.
	if _, err := repository.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	request, err = repository.GetSDKPackageBuildRequest(ctx, accountID, appID)
	// Recovery remains valid after the active snapshot's pin has changed.
	if err != nil {
		t.Fatal(err)
	}
	assertSDKPackageBuildBinding(t, request, binding)
	// Retaining generation data does not broaden account authorization.
	if _, err := repository.GetSDKPackageBuildRequest(ctx, uuid.New(), appID); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("cross-account lookup error = %v, want ErrAppNotFound", err)
	}
}

// assertSDKPackageBuildIdentity keeps app/version/idempotency checks separate from provider contract pin identity.
func assertSDKPackageBuildIdentity(t *testing.T, request *models.SDKGenerationRequest, appID, familyID, planID uuid.UUID) {
	t.Helper()
	// Reconstruction must preserve the exact app and applied-plan idempotency identity.
	if request.AppID != appID || request.AppFamilyID != familyID || request.IdempotencyKey != planID.String() {
		t.Fatalf("unexpected immutable identity: %#v", request)
	}
	// Human metadata remains tied to the applied configuration, not current service catalogue values.
	if request.Name != "Jira-SDK" || request.Version != "2.0.0" || request.Description != "Pinned Jira SDK" {
		t.Fatalf("unexpected SDK metadata: %#v", request)
	}
	// Regeneration must retain the explicitly configured execution host.
	if request.DefaultEngineURL != "https://tenant-exec.example.com:443" {
		t.Fatalf("default Engine URL=%q", request.DefaultEngineURL)
	}
}

// assertSDKPackageBuildBinding compares the original generation pin without querying a newer runtime snapshot.
func assertSDKPackageBuildBinding(t *testing.T, request *models.SDKGenerationRequest, binding models.SDKContractBinding) {
	t.Helper()
	// The selected version and the complete generation binding remain authoritative together.
	if len(request.Selections) != 1 || request.Selections[0].ServiceVersionID != binding.ServiceVersionID {
		t.Fatalf("selections=%+v", request.Selections)
	}
	// Whole-binding equality covers exact version, revision, source hash, and retained object hash.
	if len(request.ContractBindings) != 1 || request.ContractBindings[0] != binding {
		t.Fatalf("bindings=%+v", request.ContractBindings)
	}
}
