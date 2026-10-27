package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type snapshotRecoveryBridge struct {
	calls   atomic.Int32
	offline atomic.Bool
}

func (b *snapshotRecoveryBridge) AuthorizeSnapshot(context.Context, string, bridgehost.Pin, []byte) error {
	return nil
}
func (b *snapshotRecoveryBridge) Invoke(ctx context.Context, _ string, _ bridgehost.Pin, _ []byte) ([]byte, error) {
	b.calls.Add(1)
	if b.offline.Load() {
		return nil, errors.New("Bridge deliberately offline")
	}
	if bridgehost.CallIdentity(ctx) == "" {
		return nil, errors.New("missing stable identity")
	}
	return []byte(`{"id":1}`), nil
}

// TestBridgeSnapshotCompiledRecovery exercises production PostgreSQL retention and a replacement isolated worker.
func TestBridgeSnapshotCompiledRecovery(t *testing.T) {
	f := openMCPAppOperationPostgresFixture(t)
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	var reads atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if reads.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"retry"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":2}`))
	}))
	defer service.Close()
	source := `import * as z from "zod/mini";import {buildUnifiedApp} from "@fused/unified-app";import {fused} from "@fused/operations";
 export default buildUnifiedApp({snapshot:true,recovery:{mode:"manual",maxAttempts:3,backoffSeconds:1},input:z.object({}),output:z.object({first:z.number(),second:z.number()}),async execute(){
 const first=z.object({id:z.number()}).parse(await fused.callBridge("device",{}));
 const second=z.object({id:z.number()}).parse(await fused.todos.second({}));return {first:first.id,second:second.id};}});`
	pins := []executionCompilerSelection{{Service: "todos", Operation: "second", ServiceID: f.serviceID.String(), ServiceVersionID: f.serviceVersionID.String(), EndpointID: f.createEndpointID.String()}}
	artifact, err := runExecutionCompiler(f.ctx, source, pins)
	require.NoError(t, err)
	seedRecoveryApp(t, f, artifact)
	seedRecoveryLiveService(t, f)
	selection, _ := json.Marshal([]models.SDKSelection{{SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: f.serviceID, ServiceVersionID: f.serviceVersionID, EndpointIDs: []uuid.UUID{f.createEndpointID}, OperationNames: []string{"second"}}})
	_, err = f.pool.Exec(f.ctx, `UPDATE fused_apps SET selections=$2 WHERE app_id=$1`, f.appID, selection)
	require.NoError(t, err)
	snapshot := store.ServiceContractSnapshot{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(), ServiceID: f.serviceID, ServiceVersionID: f.serviceVersionID, Version: "live-test", ServiceMetadata: fusedobject.ServiceMetadata{ID: f.serviceID, ServiceVersionID: f.serviceVersionID, Name: "Retry fixture", BaseURL: service.URL}, Endpoints: []fusedobject.Endpoint{{ID: f.createEndpointID, Name: "second", Method: "GET", Path: "/", NormalizedPath: "/", SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}}}}
	_, err = f.snapshotStore.UpsertServiceContractSnapshot(f.ctx, snapshot)
	require.NoError(t, err)
	binding := models.BridgeBinding{Alias: "device", Kind: "command", BridgeReference: models.BridgeReference{PairingID: "test", ProfileID: "home", ConnectionID: "speaker", OperationID: "read", ManifestHash: "manifest"}, ContractHash: "contract"}
	payload, _ := json.Marshal(map[string]any{"bridges": []models.BridgeBinding{binding}})
	_, err = f.pool.Exec(f.ctx, `INSERT INTO fused_config_plans(config_key,source_hash,config_type,status,owner_team_id,resolved_payload,required_permissions) SELECT config_key,source_hash,'unified_app','applied',$2,$3,'[]' FROM fused_apps WHERE app_id=$1`, f.appID, f.teamID, payload)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM fused_config_plans WHERE owner_team_id=$1`, f.teamID)
	})
	_, err = f.pool.Exec(f.ctx, `INSERT INTO fused_app_capabilities(app_id,capability_key) VALUES($1,'bridge:device')`, f.appID)
	require.NoError(t, err)
	sandbox.SetIdempotencyStore(f.repository)
	t.Cleanup(func() { sandbox.SetIdempotencyStore(nil) })
	bridge := &snapshotRecoveryBridge{}
	firstServer := newRecoveryLiveServiceServer(t, f)
	firstServer.bridgeHost = bridge
	first := initialRecoveryLiveServiceRun(t, f, firstServer, "webhook")
	require.Contains(t, []string{"failed", "indeterminate"}, first.Status)
	if bridge.calls.Load() != 1 {
		detail, _ := f.repository.(store.ExecutionDiagnosticsStore).GetExecutionDiagnostics(f.ctx, f.accountID, f.appID, first.ExecutionID, firstServer.masterKey)
		t.Fatalf("first=%+v diagnostic=%s", first, detail)
	}
	require.EqualValues(t, 1, reads.Load())
	firstServer.capabilityWorkerManager().Close()
	bridge.offline.Store(true)
	replacement := newRecoveryLiveServiceServer(t, f)
	replacement.bridgeHost = bridge
	triggerManualRecovery(t, f, first.ExecutionID)
	replacement.runExecutionRecoveryBatch(f.ctx)
	attempts, err := f.repository.(store.ExecutionRecoveryStore).ListExecutionAttempts(f.ctx, f.accountID, f.appID, first.ExecutionID)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	require.Equal(t, "succeeded", attempts[1].Status)
	require.EqualValues(t, 1, bridge.calls.Load(), "Bridge must not be contacted during replay")
	require.EqualValues(t, 2, reads.Load())
	result, err := f.repository.(store.ExecutionResultStore).GetExecutionResult(f.ctx, f.accountID, f.appID, attempts[1].ExecutionID)
	require.NoError(t, err)
	require.JSONEq(t, `{"first":1,"second":2}`, string(result.Output))
	var retained int
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM fused_engine_idempotency_keys WHERE app_id=$1 AND run_id=$2 AND NOT pending`, f.appID, first.ExecutionID).Scan(&retained))
	require.Equal(t, 2, retained)
	t.Log("compiled recovery succeeded with Bridge offline: one Bridge effect, two HTTP attempts, two retained responses")
}

// Snapshot reuse still checks installation consent even when the network lease has expired.
func TestBridgeSnapshotPostgresAuthorization(t *testing.T) {
	f := openMCPAppOperationPostgresFixture(t)
	actor := seedBridgePostgresActor(t, f.ctx, f.repository)
	ref := seedBridgePostgresManifest(t, f.ctx, f.pool, actor)
	var hash string
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT convert_from(document,'UTF8')::jsonb->'operations'->0->>'contract_hash' FROM fused_bridge_manifests WHERE pairing_id=$1`, ref.PairingID).Scan(&hash))
	pin := bridgehost.Pin{PairingID: ref.PairingID, ProfileID: ref.ProfileID, ManifestHash: ref.ManifestHash, ConnectionID: ref.ConnectionID, OperationID: ref.OperationID, ContractHash: hash}
	host := bridgehost.New(f.pool, nil)
	require.NoError(t, host.AuthorizeSnapshot(f.ctx, actor.AccountID.String(), pin, []byte(`{}`)))
	_, err := host.Invoke(f.ctx, actor.AccountID.String(), pin, []byte(`{}`))
	require.Error(t, err, "expired lease must prevent a fresh effect")
	_, err = f.pool.Exec(f.ctx, `UPDATE fused_bridge_pairings SET revoked_at=now() WHERE id=$1`, ref.PairingID)
	require.NoError(t, err)
	require.Error(t, host.AuthorizeSnapshot(f.ctx, actor.AccountID.String(), pin, []byte(`{}`)))
}
