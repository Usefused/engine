package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRecoveryConsumer creates an immutable applied caller binding rather than granting target-wide access.
func seedRecoveryConsumer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, target ExecutionResult, kind string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	family, id, plan := uuid.New(), uuid.New(), uuid.New()
	name := "recovery-consumer:" + id.String()
	// Fixture cleanup removes only this caller and its plan after dependent assertions finish.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_tokens WHERE app_family_id=$1`, family)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, family)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE id=$1`, plan)
	})
	_, err := pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_team_id)
      SELECT $1,$2,$3,$4,'Recovery consumer',CASE WHEN $3='mcp' THEN NULL ELSE 'typescript' END,owner_team_id FROM fused_app_families WHERE app_family_id=$5`, family, target.AccountID, kind, name, target.AppFamilyID)
	recoveryMust(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,bundle_digest,status,selections)
      VALUES($1,$2,$3,'1.0.0',$4,'consumer-source','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','active','[]')`, id, family, target.AccountID, name)
	recoveryMust(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=$2 WHERE app_family_id=$1 AND kind='unified_app'`, family, id)
	recoveryMust(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_config_plans(id,config_key,config_type,source_hash,status,resolved_payload,required_permissions,owner_team_id)
      SELECT $1,$2,$3,'consumer-source','applied',jsonb_build_object('unified_apps',jsonb_build_array(jsonb_build_object('alias','child','app_id',a.app_id,'source_hash',a.source_hash,'bundle_digest',a.bundle_digest))),'[]',f.owner_team_id
      FROM fused_apps a JOIN fused_app_families f ON f.app_family_id=a.app_family_id WHERE a.app_id=$4`, plan, name, kind, target.AppID)
	recoveryMust(t, err)
	return family, id
}

// TestExecutionRecoveryDelegatedOrigins proves every caller adapter can recover without relaxing live authorization.
func TestExecutionRecoveryDelegatedOrigins(t *testing.T) {
	// SDK, MCP and app consumers share the same durable authorization path.
	for _, kind := range []string{"sdk", "mcp", "unified_app"} {
		// Each case retains its own token and applied caller version.
		t.Run(kind, func(t *testing.T) {
			repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
			family, caller := seedRecoveryConsumer(t, ctx, pool, record, kind)
			record.SourceWebhookEventID = ""
			record.AppTokenID = uuid.New()
			record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 3, BackoffSeconds: 1}
			record.RecoveryOrigin = &ExecutionRecoveryOrigin{AppID: caller, Calls: []ExecutionRecoveryCall{{CallerID: caller, TargetID: record.AppID, Alias: "child"}}}
			_, err := repository.CreateAppToken(ctx, AppTokenIssue{ID: record.AppTokenID, AppFamilyID: family, TokenHash: strings.ReplaceAll(record.AppTokenID.String(), "-", "") + strings.Repeat("a", 32), Name: "recovery", BindingMode: AppTokenBindingDynamic, Policy: AppTokenPolicy{AllowedOperations: []string{"unified_app:child"}}})
			recoveryMust(t, err)
			recoveryMust(t, repository.CreateExecutionResult(ctx, record))
			_, err = repository.SaveExecutionSnapshot(ctx, record.AccountID, record.AppID, record.ID, 0, json.RawMessage(`{"next":2}`), bytes.Repeat([]byte{7}, 32))
			recoveryMust(t, err)
			// A replacement store has no cached caller identity or original bearer credential.
			replacement := NewPostgresStore(pool).(*postgresStore)
			projection, err := replacement.AuthorizeExecutionRecovery(ctx, record.AccountID, record.AppID, record.ID)
			recoveryMust(t, err)
			// Alias grants authorize the target's provider scope, not the root's physical operations.
			if !projection.TokenPolicy.AllowAll || projection.TokenID != record.AppTokenID {
				t.Fatal("delegated execution identity changed")
			}
			_, err = pool.Exec(ctx, `UPDATE fused_unified_app_recovery SET lease_until=NOW()-INTERVAL '1 second' WHERE execution_id=$1`, record.ID)
			recoveryMust(t, err)
			due, err := replacement.DueExecutionRecoveries(ctx)
			recoveryMust(t, err)
			// The scheduler must discover delegated work after losing its first process.
			if len(due) != 1 || due[0].ID != record.ID {
				t.Fatalf("due=%+v", due)
			}
			next, created, err := replacement.ReserveExecutionAttempt(ctx, ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: record.ID, TokenID: record.AppTokenID, KeyHash: strings.Repeat("b", 64), Automatic: true})
			recoveryMust(t, err)
			// Reservation must retain ancestry and the latest durable continuation.
			if !created || next.RecoveryOrigin == nil || next.RecoveryOrigin.AppID != caller {
				t.Fatal("caller ancestry lost")
			}
			snapshot, err := replacement.ReadExecutionSnapshot(ctx, next.AccountID, next.AppID, next.ID, bytes.Repeat([]byte{7}, 32))
			recoveryMust(t, err)
			if snapshot.Revision != 1 {
				t.Fatal("delegated snapshot lost")
			} // Saved work survives process replacement.
			_, err = pool.Exec(ctx, `UPDATE fused_app_tokens SET allowed_operations=ARRAY['unified_app:other'] WHERE id=$1`, record.AppTokenID)
			recoveryMust(t, err)
			// Revoking only the alias grant must fence effects even while the token remains valid.
			if err := replacement.CheckExecutionAttempt(ctx, next.AccountID, next.AppID, next.ID); err == nil {
				t.Fatal("removed alias still authorized")
			}
			_, err = replacement.AuthorizeExecutionRecovery(ctx, next.AccountID, next.AppID, next.ID)
			if err == nil {
				t.Fatal("removed alias authorized recovery")
			} // Background admission uses the same live predicate.
			_, err = pool.Exec(ctx, `UPDATE fused_app_tokens SET allowed_operations=ARRAY['unified_app:child'] WHERE id=$1`, record.AppTokenID)
			recoveryMust(t, err)
			_, err = pool.Exec(ctx, `UPDATE fused_apps SET status='building' WHERE app_id=$1`, caller)
			recoveryMust(t, err)
			// A retained target cannot execute on behalf of a caller that is no longer runnable.
			if err := replacement.CheckExecutionAttempt(ctx, next.AccountID, next.AppID, next.ID); err == nil {
				t.Fatal("retired caller authorized")
			}
		})
	}
}

// TestExecutionRecoveryNestedAutomation preserves tokenless root provenance without assigning child webhook deliveries.
func TestExecutionRecoveryNestedAutomation(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	middleFamily, middle := seedRecoveryConsumer(t, ctx, pool, record, "unified_app")
	parent := record
	parent.AppID, parent.AppFamilyID = middle, middleFamily
	_, root := seedRecoveryConsumer(t, ctx, pool, parent, "unified_app")
	record.SourceWebhookEventID = ""
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 3, BackoffSeconds: 1}
	record.RecoveryOrigin = &ExecutionRecoveryOrigin{AppID: root, WebhookEventID: uuid.NewString(), Calls: []ExecutionRecoveryCall{{CallerID: root, TargetID: middle, Alias: "child"}, {CallerID: middle, TargetID: record.AppID, Alias: "child"}}}
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	_, err := repository.AuthorizeExecutionRecovery(ctx, record.AccountID, record.AppID, record.ID)
	recoveryMust(t, err)
	// Retirement anywhere in the retained chain must fence the descendant, not just its immediate parent.
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=NULL WHERE unified_active_app_id=$1`, root)
	recoveryMust(t, err)
	if err := repository.CheckExecutionAttempt(ctx, record.AccountID, record.AppID, record.ID); err == nil {
		t.Fatal("retired root retained descendant authority")
	}
	// A malformed chain cannot be admitted merely because every referenced app exists.
	record.ID = uuid.New()
	record.RecoveryOrigin.Calls[1].CallerID = root
	if err := repository.CreateExecutionResult(ctx, record); err == nil {
		t.Fatal("disconnected ancestry admitted")
	}
}
