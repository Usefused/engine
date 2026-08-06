package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestGetSDKPackageBuildRequestUsesExactAppliedPlan(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
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
	if err != nil {
		t.Fatalf("create SDK family: %v", err)
	}
	selection := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointIDs: []uuid.UUID{uuid.New()}}
	selections, _ := json.Marshal([]models.SDKSelection{selection})
	sourceHash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	configKey := "sdk:Jira-SDK:2.0.0"
	if _, _, err := repository.PublishAppVersion(ctx, App{
		AppID: appID, AppFamilyID: family.AppFamilyID, AccountID: accountID,
		Version: "2.0.0", ConfigKey: configKey, SourceHash: sourceHash,
		CapabilityHash: "capability", ScopeSchemaVersion: models.AppScopeSchemaVersion,
		Selections: selections, GeneratorVersion: models.SDKGeneratorVersion, Status: "deprecated",
	}); err != nil {
		t.Fatalf("publish SDK app: %v", err)
	}
	binding := models.SDKContractBinding{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID, Version: "2026-01", Revision: 7, SourceHash: "contract"}
	resolved, _ := json.Marshal(map[string]any{
		"description": "Pinned Jira SDK", "contract_bindings": []models.SDKContractBinding{binding},
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO fused_config_plans
			(id, config_key, config_type, owner_team_id, source_hash, status,
			 actions, desired_state, resolved_payload, blockers, warnings,
			 required_permissions, applied_at)
		VALUES ($1,$2,'sdk',$3,$4,'applied','[]','{}',$5,'[]','[]','[]',NOW())
	`, planID, configKey, teamID, sourceHash, resolved)
	if err != nil {
		t.Fatalf("seed applied SDK plan: %v", err)
	}

	request, err := repository.GetSDKPackageBuildRequest(ctx, accountID, appID)
	if err != nil {
		t.Fatalf("GetSDKPackageBuildRequest() error = %v", err)
	}
	if request.AppID != appID || request.AppFamilyID != familyID || request.IdempotencyKey != planID.String() {
		t.Fatalf("unexpected immutable identity: %#v", request)
	}
	if request.Name != "Jira-SDK" || request.Version != "2.0.0" || request.Description != "Pinned Jira SDK" {
		t.Fatalf("unexpected SDK metadata: %#v", request)
	}
	if len(request.Selections) != 1 || request.Selections[0].ServiceVersionID != selection.ServiceVersionID || len(request.ContractBindings) != 1 || request.ContractBindings[0].Revision != 7 {
		t.Fatalf("unexpected pinned definition: %#v", request)
	}
	if _, err := repository.GetSDKPackageBuildRequest(ctx, uuid.New(), appID); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("cross-account lookup error = %v, want ErrAppNotFound", err)
	}
}
