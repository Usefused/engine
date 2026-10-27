package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestUnifiedAppDelegatedRecoveryProcess resumes compiled automatic apps through every consumer adapter after restart.
func TestUnifiedAppDelegatedRecoveryProcess(t *testing.T) {
	// These adapters all enter the shared attachment executor with their authenticated consumer identity.
	for _, kind := range []store.AppKind{store.AppKindSDK, store.AppKindMCP, store.AppKindUnifiedApp} {
		// Each adapter has its own exact applied version and restricted alias token.
		t.Run(string(kind), func(t *testing.T) {
			fixture := openMCPAppOperationPostgresFixture(t)
			t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
			artifact, err := runExecutionCompiler(fixture.ctx, recoveryTestSource("automatic"), []executionCompilerSelection{})
			recoveryAPIMust(t, err)
			seedRecoveryApp(t, fixture, artifact)
			caller := seedDelegatedRecoveryCaller(t, fixture, kind)
			server := &EngineGRPCServer{store: fixture.repository, masterKey: bytes.Repeat([]byte{7}, 32)}
			t.Cleanup(server.capabilityWorkerManager().Close)
			raw, err := server.ExecuteAttachedUnifiedApp(fixture.ctx, caller, "child", json.RawMessage(`{}`))
			recoveryAPIMust(t, err)
			var first capabilityExecutionEnvelope
			recoveryAPIMust(t, json.Unmarshal(raw, &first))
			// Admission must reach authored code, which fails only after its first durable save.
			if first.Status != "failed" || first.Attempt != 1 {
				t.Fatalf("first=%s", raw)
			}
			server.capabilityWorkerManager().Close()
			_, err = fixture.pool.Exec(fixture.ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '2 seconds' WHERE id=$1`, first.ExecutionID)
			recoveryAPIMust(t, err)
			replacement := &EngineGRPCServer{store: store.NewPostgresStore(fixture.pool), masterKey: bytes.Repeat([]byte{7}, 32)}
			t.Cleanup(replacement.capabilityWorkerManager().Close)
			replacement.runExecutionRecoveryBatch(fixture.ctx)
			attempts, err := fixture.repository.(store.ExecutionRecoveryStore).ListExecutionAttempts(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
			recoveryAPIMust(t, err)
			// A fresh process must recover the saved prefix once under the original consumer authority.
			if len(attempts) != 2 || attempts[1].Status != "succeeded" || attempts[1].SnapshotRevision != 5 {
				t.Fatalf("attempts=%+v", attempts)
			}
			result, err := fixture.repository.(store.ExecutionResultStore).GetExecutionResult(fixture.ctx, fixture.accountID, fixture.appID, attempts[1].ExecutionID)
			recoveryAPIMust(t, err)
			var output struct {
				Sum int `json:"sum"`
			}
			recoveryAPIMust(t, json.Unmarshal(result.Output, &output))
			// Compare the result semantically because PostgreSQL normalizes JSON whitespace.
			if output.Sum != 15 {
				t.Fatalf("output=%s", result.Output)
			} // Repeated or skipped prefix work changes the sum.
		})
	}
}

// seedDelegatedRecoveryCaller installs a real applied attachment and token without exposing a target token to callers.
func seedDelegatedRecoveryCaller(t *testing.T, fixture mcpOperationPostgresFixture, kind store.AppKind) auth.RuntimeIdentity {
	t.Helper()
	family, id, plan, token := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	name := "recovery-caller:" + id.String()
	// Caller-owned rows are removed before the enclosing target and owner fixture closes.
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_app_tokens WHERE id=$1`, token)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_id=$1`, id)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, family)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE id=$1`, plan)
	})
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_team_id)
      VALUES($1,$2,$3,$4,'Recovery caller',CASE WHEN $3='mcp' THEN NULL ELSE 'typescript' END,$5)`, family, fixture.accountID, string(kind), name, fixture.teamID)
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status,selections)
      VALUES($1,$2,$3,'1.0.0',$4,'recovery-caller','active','[]')`, id, family, fixture.accountID, name)
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `UPDATE fused_app_families SET unified_active_app_id=$2 WHERE app_family_id=$1 AND kind='unified_app'`, family, id)
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_config_plans(id,config_key,config_type,source_hash,status,resolved_payload,required_permissions,owner_team_id)
      SELECT $1,$2,$3,'recovery-caller','applied',jsonb_build_object('unified_apps',jsonb_build_array(jsonb_build_object('alias','child','app_id',app_id,'app_family_id',app_family_id,'source_hash',source_hash,'bundle_digest',bundle_digest))),'[]',$4 FROM fused_apps WHERE app_id=$5`, plan, name, string(kind), fixture.teamID, fixture.appID)
	recoveryAPIMust(t, err)
	policy := store.AppTokenPolicy{AllowedOperations: []string{"unified_app:child"}}
	_, err = fixture.repository.CreateAppToken(fixture.ctx, store.AppTokenIssue{ID: token, AppFamilyID: family, TokenHash: strings.ReplaceAll(token.String(), "-", "") + strings.Repeat("a", 32), Name: "consumer", BindingMode: store.AppTokenBindingDynamic, Policy: policy})
	recoveryAPIMust(t, err)
	return auth.RuntimeIdentity{AccountID: fixture.accountID, AppID: id, AppFamilyID: family, TokenID: token, AppVersion: "1.0.0", Kind: kind, Status: store.AppStatusActive, TokenPolicy: policy}
}
