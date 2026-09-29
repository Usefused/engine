package store

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

// assertUnifiedAppAttachmentResolution proves account isolation and exact active-version binding with real SQL.
func assertUnifiedAppAttachmentResolution(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID, familyID uuid.UUID) {
	t.Helper()
	var version string
	var activeID uuid.UUID
	// The test follows the persisted pointer rather than relying on publication order.
	if err := pool.QueryRow(ctx, `SELECT a.app_id,a.version FROM fused_apps a JOIN fused_app_families f ON f.unified_active_app_id=a.app_id WHERE f.app_family_id=$1`, familyID).Scan(&activeID, &version); err != nil {
		t.Fatal(err)
	}
	repository := NewPostgresStore(pool).(UnifiedAppAttachmentStore)
	refs := map[string]models.UnifiedAppReference{"lookup": {Name: "bundle-" + familyID.String(), Version: version}}
	bindings, err := repository.ResolveUnifiedAppBindings(ctx, accountID, refs)
	if err != nil || len(bindings) != 1 || bindings[0].AppID != activeID || bindings[0].Alias != "lookup" {
		t.Fatalf("bindings=%v error=%v", bindings, err)
	}
	assertUnifiedAppConsumerRecovery(t, ctx, pool, accountID, familyID, bindings)
	// Another account must not discover or reuse the same named capability.
	if _, err := repository.ResolveUnifiedAppBindings(ctx, uuid.New(), refs); err == nil {
		t.Fatal("cross-account attachment resolved")
	}
	refs["lookup"] = models.UnifiedAppReference{Name: "bundle-" + familyID.String(), Version: "9.9.9"}
	if _, err := repository.ResolveUnifiedAppBindings(ctx, accountID, refs); err == nil {
		t.Fatal("missing version resolved")
	}
}

// assertUnifiedAppConsumerRecovery proves runtime reads, package redownload, and pending recovery share applied bindings.
func assertUnifiedAppConsumerRecovery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID, targetFamilyID uuid.UUID, bindings []models.UnifiedAppBinding) {
	t.Helper()
	consumerFamily, consumerID, planID := uuid.New(), uuid.New(), uuid.New()
	key := "attachment-test:" + consumerID.String()
	payload, err := json.Marshal(map[string]any{"unified_apps": bindings})
	if err != nil {
		t.Fatal(err)
	}
	// Every inserted row belongs exclusively to this fixture and is removed before the parent owner cleanup.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_id=$1`, consumerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, consumerFamily)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE id=$1`, planID)
	})
	_, err = pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,delivery_mode,owner_team_id) SELECT $1,$2,'sdk',$3,'Attachment consumer','typescript','sdk',owner_team_id FROM fused_app_families WHERE app_family_id=$4`, consumerFamily, accountID, key, targetFamilyID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status,selections) VALUES($1,$2,$3,'1.0.0',$4,'attachment-source','active','[]')`, consumerID, consumerFamily, accountID, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_config_plans(id,config_key,config_type,source_hash,status,resolved_payload,required_permissions,owner_team_id) SELECT $1,$2,'sdk','attachment-source','applied',$3,'[]',owner_team_id FROM fused_app_families WHERE app_family_id=$4`, planID, key, payload, targetFamilyID)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewPostgresStore(pool).(*postgresStore)
	read, err := repository.ReadUnifiedAppBindings(ctx, accountID, consumerID)
	if err != nil || len(read) != 1 || read[0].AppID != bindings[0].AppID {
		t.Fatalf("runtime bindings=%v error=%v", read, err)
	}
	request, err := repository.GetSDKPackageBuildRequest(ctx, accountID, consumerID)
	if err != nil || len(request.UnifiedApps) != 1 || request.UnifiedApps[0].AppID != bindings[0].AppID {
		t.Fatalf("package request=%v error=%v", request, err)
	}
	build, err := repository.GetSDKGenerationBuild(ctx, accountID, consumerID)
	if err != nil || len(build.Request.UnifiedApps) != 1 || build.Request.UnifiedApps[0].AppID != bindings[0].AppID {
		t.Fatalf("recovery build=%v error=%v", build, err)
	}
	// A target name or applied-plan record never authorizes a different account's consumer.
	if _, err := repository.ReadUnifiedAppBindings(ctx, uuid.New(), consumerID); err == nil {
		t.Fatal("cross-account consumer readable")
	}
}
