package api

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/stretchr/testify/require"
)

type notStartedRecoveryBridge struct {
	offline atomic.Bool
	effects atomic.Int32
}

// AuthorizeSnapshot keeps this fixture focused on dispatch outcome classification.
func (b *notStartedRecoveryBridge) AuthorizeSnapshot(context.Context, string, bridgehost.Pin, []byte) error {
	return nil
}

// Invoke rejects before any effect while offline and counts only actual effects after recovery.
func (b *notStartedRecoveryBridge) Invoke(ctx context.Context, _ string, _ bridgehost.Pin, _ []byte) ([]byte, error) {
	// A positively known pre-dispatch refusal must not become an uncertain side effect.
	if b.offline.Load() {
		return nil, &bridgehost.InvocationError{Outcome: "not_started", Code: "unavailable"}
	}
	b.effects.Add(1)
	return []byte(`{"id":1}`), nil
}

// TestBridgeSnapshotCompiledNotStartedRecovery exercises the real compiled worker status and manual recovery path after a known pre-dispatch Bridge refusal.
func TestBridgeSnapshotCompiledNotStartedRecovery(t *testing.T) {
	f := openMCPAppOperationPostgresFixture(t)
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	source := `import * as z from "zod/mini";import {buildUnifiedApp} from "@fused/unified-app";import {fused} from "@fused/operations";
 export default buildUnifiedApp({snapshot:true,recovery:{mode:"manual",maxAttempts:3,backoffSeconds:1},input:z.object({}),output:z.object({id:z.number()}),async execute(){return z.object({id:z.number()}).parse(await fused.callBridge("device",{}));}});`
	artifact, err := runExecutionCompiler(f.ctx, source, []executionCompilerSelection{})
	require.NoError(t, err)
	seedRecoveryApp(t, f, artifact)
	binding := models.BridgeBinding{Alias: "device", Kind: "read", BridgeReference: models.BridgeReference{PairingID: "test", ProfileID: "home", ConnectionID: "speaker", OperationID: "read", ManifestHash: "manifest"}, ContractHash: "contract"}
	payload, _ := json.Marshal(map[string]any{"bridges": []models.BridgeBinding{binding}})
	_, err = f.pool.Exec(f.ctx, `INSERT INTO fused_config_plans(config_key,source_hash,config_type,status,owner_team_id,resolved_payload,required_permissions) SELECT config_key,source_hash,'unified_app','applied',$2,$3,'[]' FROM fused_apps WHERE app_id=$1`, f.appID, f.teamID, payload)
	require.NoError(t, err)
	// Cleanup affects only this fixture's authored selection plan.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE owner_team_id=$1`, f.teamID)
	})
	_, err = f.pool.Exec(f.ctx, `INSERT INTO fused_app_capabilities(app_id,capability_key) VALUES($1,'bridge:device')`, f.appID)
	require.NoError(t, err)
	sandbox.SetIdempotencyStore(f.repository)
	t.Cleanup(func() { sandbox.SetIdempotencyStore(nil) })
	bridge := &notStartedRecoveryBridge{}
	bridge.offline.Store(true)
	firstServer := newRecoveryLiveServiceServer(t, f)
	firstServer.bridgeHost = bridge
	first := initialRecoveryLiveServiceRun(t, f, firstServer, "webhook")
	require.Equal(t, "failed", first.Status, "known not_started must not report indeterminate")
	require.Zero(t, bridge.effects.Load())
	firstServer.capabilityWorkerManager().Close()
	bridge.offline.Store(false)
	replacement := newRecoveryLiveServiceServer(t, f)
	replacement.bridgeHost = bridge
	triggerManualRecovery(t, f, first.ExecutionID)
	replacement.runExecutionRecoveryBatch(f.ctx)
	attempts, err := f.repository.(store.ExecutionRecoveryStore).ListExecutionAttempts(f.ctx, f.accountID, f.appID, first.ExecutionID)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	require.Equal(t, "succeeded", attempts[1].Status)
	require.EqualValues(t, 1, bridge.effects.Load())
	t.Log("known-not-started compiled attempt failed without effects; replacement manual recovery succeeded with exactly one effect")
}
