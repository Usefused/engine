package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// recoveryAPIMust makes fixture failures distinguishable from authored runtime failures.
func recoveryAPIMust(t *testing.T, err error) {
	t.Helper()
	// A failed prerequisite cannot support an execution or durability assertion.
	if err != nil {
		t.Fatal(err)
	}
}

// TestUnifiedAppSnapshotRecoveryProcess uses compiled TypeScript, the production schema and real confined executors.
func TestUnifiedAppSnapshotRecoveryProcess(t *testing.T) {
	// Both trigger modes must recover exactly the same saved continuation.
	for _, mode := range []string{"manual", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			fixture := openMCPAppOperationPostgresFixture(t)
			// Explicit database integration must exercise confinement rather than silently substituting an interpreter.
			if !sandbox.IsCapabilityWorkerAvailable(fixture.ctx) {
				t.Fatal("confined executor unavailable for recovery integration")
			}
			t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
			artifact, err := runExecutionCompiler(fixture.ctx, recoveryTestSource(mode), []executionCompilerSelection{})
			recoveryAPIMust(t, err)
			seedRecoveryApp(t, fixture, artifact)
			server := &EngineGRPCServer{store: fixture.repository, masterKey: bytes.Repeat([]byte{7}, 32)}
			t.Cleanup(server.capabilityWorkerManager().Close)
			bundle, manifest, _, requestErr := server.findUnifiedAppBundle(fixture.ctx, fixture.appID)
			// Bundle identity and manifest validation must pass before testing authored execution.
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			identity := auth.RuntimeIdentity{AccountID: fixture.accountID, AppFamilyID: fixture.familyID, AppID: fixture.appID, AppVersion: "1.0.0", Kind: store.AppKindUnifiedApp}
			first, requestErr := server.executeCapabilityRun(fixture.ctx, capabilityRunSpec{identity: identity, version: "1.0.0", bundle: bundle, manifest: manifest, input: json.RawMessage(`{}`), mode: "live", transport: "webhook", webhookEventID: uuid.NewString()})
			// The first process must fail only after committing its authored continuation.
			if requestErr != nil || first.Status != "failed" || first.Attempt != 1 {
				t.Fatalf("first=%+v err=%v", first, requestErr)
			}
			repository := fixture.repository.(store.ExecutionRecoveryStore)
			state, err := repository.GetExecutionRecovery(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
			recoveryAPIMust(t, err)
			if state.Revision != 1 {
				t.Fatalf("snapshot revision=%d", state.Revision)
			} // Failure must not roll back the acknowledged save.
			t.Logf("run=%s first attempt failed after durable snapshot revision=1", first.ExecutionID)
			server.capabilityWorkerManager().Close()
			replacement := &EngineGRPCServer{store: store.NewPostgresStore(fixture.pool), masterKey: bytes.Repeat([]byte{7}, 32)}
			t.Cleanup(replacement.capabilityWorkerManager().Close)
			recoverCompiledAttempt(t, fixture, replacement, first, identity, mode)
		})
	}
}

// recoveryTestSource proves a replacement receives saved progress without an authored load call.
func recoveryTestSource(mode string) string {
	return fmt.Sprintf(`import * as z from "zod/mini";
import {buildUnifiedApp,snapshot} from "@fused/unified-app";
export default buildUnifiedApp({input:z.object({}),output:z.object({sum:z.number()}),
recovery:{mode:%q,maxAttempts:3,backoffSeconds:1},
// Engine supplies restored progress before a replacement enters authored execution.
async execute({snapshot:restored}){const state=z.nullable(z.object({next:z.number(),sum:z.number()})).parse(restored);
// A fresh run deliberately fails only after Engine acknowledges the durable save.
if(state===null){await snapshot.save({next:2,sum:1});throw new Error("deliberate failure after snapshot");}
let sum=state.sum;
// Resumed work skips the committed prefix and saves each completed unit.
for(let i=state.next;i<=5;i++){sum+=i;await snapshot.save({next:i+1,sum});}
return {sum};}});`, mode)
}

// seedRecoveryApp installs only fixture-owned immutable identities through the real bundle persistence boundary.
func seedRecoveryApp(t *testing.T, fixture mcpOperationPostgresFixture, artifact *executionPlanArtifact) {
	t.Helper()
	// Results and bundles must be deleted before the enclosing identity fixture closes its pool.
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_unified_app_results WHERE app_id=$1`, fixture.appID)
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_unified_app_bundles WHERE app_id=$1`, fixture.appID)
	})
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_teams(id,name,slug) VALUES($1,'Recovery test',$2)`, fixture.teamID, "recovery-"+fixture.teamID.String())
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_team_id) VALUES($1,$2,'unified_app',$3,'Recovery test','typescript',$4)`, fixture.familyID, fixture.accountID, "recovery-"+fixture.familyID.String(), fixture.teamID)
	recoveryAPIMust(t, err)
	sourceHash := "sha256:" + strings.Repeat("a", 64)
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,bundle_digest,status,scope_schema_version,selections) VALUES($1,$2,$3,'1.0.0',$4,$5,$6,'active',$7,'[]')`, fixture.appID, fixture.familyID, fixture.accountID, "unified_app:recovery:"+fixture.appID.String(), sourceHash, artifact.Digest, models.AppScopeSchemaVersion)
	recoveryAPIMust(t, err)
	recoveryAPIMust(t, fixture.repository.(store.UnifiedAppBundleStore).CreateUnifiedAppBundle(fixture.ctx, store.UnifiedAppBundle{AppID: fixture.appID, SourceHash: sourceHash, BundleJS: artifact.BundleJS, Manifest: artifact.Manifest}))
}

// recoverCompiledAttempt drives the selected trigger and checks PostgreSQL after the replacement process exits.
func recoverCompiledAttempt(t *testing.T, fixture mcpOperationPostgresFixture, server *EngineGRPCServer, first capabilityExecutionEnvelope, identity auth.RuntimeIdentity, mode string) {
	t.Helper()
	results := fixture.repository.(store.ExecutionResultStore)
	repository := fixture.repository.(store.ExecutionRecoveryStore)
	source, err := results.GetExecutionResult(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
	recoveryAPIMust(t, err)
	// Automatic recovery must pass the real scheduler's delay and policy gate.
	if mode == "automatic" {
		time.Sleep(1100 * time.Millisecond)
		server.runExecutionRecoveryBatch(fixture.ctx)
	} else {
		// Manual policy must remain idle even when the same scheduler polls for work.
		server.runExecutionRecoveryBatch(fixture.ctx)
		history, err := repository.ListExecutionAttempts(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
		recoveryAPIMust(t, err)
		if len(history) != 1 {
			t.Fatal("manual run automatically retried")
		} // No manual continuation may start without a trigger.
		triggerManualRecovery(t, fixture, source.ID)
		server.runExecutionRecoveryBatch(fixture.ctx)
	}
	history, err := repository.ListExecutionAttempts(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
	recoveryAPIMust(t, err)
	// One original and one replacement must share a logical run and preserve five committed progress saves.
	if len(history) != 2 || history[1].Status != "succeeded" || history[1].SnapshotRevision != 5 {
		t.Fatalf("attempt history=%+v", history)
	}
	final, err := results.GetExecutionResult(fixture.ctx, fixture.accountID, fixture.appID, history[1].ExecutionID)
	recoveryAPIMust(t, err)
	var output struct {
		Sum int `json:"sum"`
	}
	recoveryAPIMust(t, json.Unmarshal(final.Output, &output))
	if output.Sum != 15 {
		t.Fatalf("resumed output=%s", final.Output)
	} // Repeating or skipping saved work changes the sum.
	t.Logf("run=%s replacement=%s attempt=2 snapshot_revision=5 sum=%d", first.ExecutionID, final.ID, output.Sum)
}

// triggerManualRecovery exercises the management API used for cron and webhook executions without read handles.
func triggerManualRecovery(t *testing.T, fixture mcpOperationPostgresFixture, id uuid.UUID) {
	t.Helper()
	router := newControlTestRouter(fixture.accountID)
	router.Post("/apps/{app_id}/executions/{execution_id}/rerun", UnifiedAppRecoveryControlHandler(fixture.repository))
	request := httptest.NewRequest(http.MethodPost, "/apps/"+fixture.appID.String()+"/executions/"+id.String()+"/rerun", nil)
	request.Header.Set("Idempotency-Key", "manual-recovery")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// Success means durable queue admission, not a second in-process execution runtime.
	if response.Code != http.StatusAccepted {
		t.Fatalf("manual trigger status=%d body=%s", response.Code, response.Body.String())
	}
}
