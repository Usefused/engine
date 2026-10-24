package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TestUnifiedAppRecoveryLiveService opts into real HTTPS calls without exposing credentials or mutating provider data.
func TestUnifiedAppRecoveryLiveService(t *testing.T) {
	// Ordinary regression runs must not depend on an external public service's availability.
	if os.Getenv("FUSED_RECOVERY_LIVE_SERVICE") != "1" {
		t.Skip("FUSED_RECOVERY_LIVE_SERVICE=1 enables real JSONPlaceholder requests")
	}
	// Both recovery triggers must preserve provider results across a replacement interpreter.
	for _, mode := range []string{"manual", "automatic"} {
		t.Run(mode, func(t *testing.T) { runRecoveryLiveService(t, mode) })
	}
}

// runRecoveryLiveService uses production persistence, exact operation resolution, dispatch and confinement end to end.
func runRecoveryLiveService(t *testing.T, mode string) {
	t.Helper()
	fixture := openMCPAppOperationPostgresFixture(t)
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	pins := []executionCompilerSelection{
		{Service: "todos", Operation: "first", ServiceID: fixture.serviceID.String(), ServiceVersionID: fixture.serviceVersionID.String(), EndpointID: fixture.listEndpointID.String()},
		{Service: "todos", Operation: "second", ServiceID: fixture.serviceID.String(), ServiceVersionID: fixture.serviceVersionID.String(), EndpointID: fixture.createEndpointID.String()},
	}
	artifact, err := runExecutionCompiler(fixture.ctx, recoveryLiveServiceSource(mode), pins)
	recoveryAPIMust(t, err)
	seedRecoveryApp(t, fixture, artifact)
	seedRecoveryLiveService(t, fixture)
	server := newRecoveryLiveServiceServer(t, fixture)
	bundle, manifest, _, requestErr := server.findUnifiedAppBundle(fixture.ctx, fixture.appID)
	// A missing compiled bundle cannot provide evidence of a real provider invocation.
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	identity := auth.RuntimeIdentity{AccountID: fixture.accountID, AppFamilyID: fixture.familyID, AppID: fixture.appID, AppVersion: "1.0.0", Kind: store.AppKindUnifiedApp, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	first, requestErr := server.executeCapabilityRun(fixture.ctx, capabilityRunSpec{
		identity: identity, version: "1.0.0", bundle: bundle, manifest: manifest,
		input: json.RawMessage(`{}`), mode: "live", transport: "webhook", webhookEventID: uuid.NewString(),
	})
	// The first result must fail after its successful remote response was durably saved.
	if requestErr != nil || first.Status != "failed" {
		t.Fatalf("first attempt=%+v error=%v", first, requestErr)
	}
	assertRecoveryLiveServiceCall(t, fixture, first.ExecutionID, "first", 1)
	state, err := fixture.repository.(store.ExecutionRecoveryStore).GetExecutionRecovery(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
	recoveryAPIMust(t, err)
	// The deliberate failure is meaningful only after the provider result reached PostgreSQL.
	if state.Revision != 1 {
		t.Fatalf("provider response was not saved: revision=%d", state.Revision)
	}
	server.capabilityWorkerManager().Close()
	replacement := newRecoveryLiveServiceServer(t, fixture)
	resumeRecoveryLiveService(t, fixture, replacement, first, mode)
}

// recoveryLiveServiceSource saves the first real provider response so recovery needs only the second operation.
func recoveryLiveServiceSource(mode string) string {
	return fmt.Sprintf(`import * as z from "zod/mini";
import {buildUnifiedApp,snapshot} from "@fused/unified-app";
import {fused} from "@fused/operations";
const todo=z.object({id:z.number(),title:z.string()});
export default buildUnifiedApp({input:z.object({}),output:z.object({first:todo,second:todo}),
recovery:{mode:%q,maxAttempts:3,backoffSeconds:1},
// The saved provider response supplies completed work to a fresh interpreter.
async execute(){const saved=await snapshot.load<{first:{id:number,title:string}}>();
// A fresh attempt fails only after the first HTTPS response has been committed.
if(saved===null){const first=todo.parse(await fused.todos.first({}));await snapshot.save({first});throw new Error("deliberate failure after service response snapshot");}
const second=todo.parse(await fused.todos.second({}));return {first:saved.first,second};}});`, mode)
}

// seedRecoveryLiveService persists exact anonymous GET contracts and app scope in the isolated PostgreSQL fixture.
func seedRecoveryLiveService(t *testing.T, fixture mcpOperationPostgresFixture) {
	t.Helper()
	snapshot := store.ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Version: "live-test",
		ServiceMetadata: fusedobject.ServiceMetadata{ID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Name: "JSONPlaceholder", BaseURL: "https://jsonplaceholder.typicode.com"},
		Endpoints: []fusedobject.Endpoint{
			{ID: fixture.listEndpointID, Name: "first", Method: "GET", Path: "/todos/1", NormalizedPath: "/todos/1", SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}},
			{ID: fixture.createEndpointID, Name: "second", Method: "GET", Path: "/todos/2", NormalizedPath: "/todos/2", SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}},
		},
	}
	_, err := fixture.snapshotStore.UpsertServiceContractSnapshot(fixture.ctx, snapshot)
	recoveryAPIMust(t, err)
	selections, err := json.Marshal([]models.SDKSelection{{SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, EndpointIDs: []uuid.UUID{fixture.listEndpointID, fixture.createEndpointID}, OperationNames: []string{"first", "second"}}})
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `UPDATE fused_apps SET selections=$2 WHERE app_id=$1`, fixture.appID, selections)
	recoveryAPIMust(t, err)
	bucketID := uuid.New()
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, bucketID, "recovery-live-"+bucketID.String())
	recoveryAPIMust(t, err)
	// Cleanup is scoped to generated identities and removes bindings before their bucket.
	t.Cleanup(func() {
		_, err := fixture.pool.Exec(context.Background(), `DELETE FROM fused_app_family_buckets WHERE app_family_id=$1`, fixture.familyID)
		recoveryAPIMust(t, err)
		_, err = fixture.pool.Exec(context.Background(), `DELETE FROM fused_buckets WHERE id=$1`, bucketID)
		recoveryAPIMust(t, err)
	})
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_app_family_buckets(app_family_id,bucket_id) VALUES($1,$2)`, fixture.familyID, bucketID)
	recoveryAPIMust(t, err)
}

// newRecoveryLiveServiceServer replaces both worker and runtime cache while keeping only durable PostgreSQL state.
func newRecoveryLiveServiceServer(t *testing.T, fixture mcpOperationPostgresFixture) *EngineGRPCServer {
	t.Helper()
	repository := store.NewPostgresStore(fixture.pool)
	key := bytes.Repeat([]byte{7}, 32)
	validator := auth.NewTokenValidator(repository)
	sandbox.InitSandbox(chi.NewRouter(), nil, &config.Config{}, sandbox.NewLocalObjectCache(repository), validator, nil, nil, nil, sandbox.NewSecretResolver(repository, key), nil, "", nil)
	server := NewEngineGRPCServer(repository, nil, key, nil, nil, validator, nil)
	t.Cleanup(server.capabilityWorkerManager().Close)
	return server
}

// resumeRecoveryLiveService verifies the saved first response and a fresh second response in a separate attempt.
func resumeRecoveryLiveService(t *testing.T, fixture mcpOperationPostgresFixture, server *EngineGRPCServer, first capabilityExecutionEnvelope, mode string) {
	t.Helper()
	// Automatic retry honors the configured delay; manual retry must use the management queue.
	if mode == "automatic" {
		time.Sleep(1100 * time.Millisecond)
	} else {
		triggerManualRecovery(t, fixture, first.ExecutionID)
	}
	server.runExecutionRecoveryBatch(fixture.ctx)
	history, err := fixture.repository.(store.ExecutionRecoveryStore).ListExecutionAttempts(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
	recoveryAPIMust(t, err)
	// Exactly one replacement must finish successfully without starting another logical run.
	if len(history) != 2 || history[1].Status != "succeeded" {
		t.Fatalf("attempt history=%+v", history)
	}
	assertRecoveryLiveServiceCall(t, fixture, history[1].ExecutionID, "second", 2)
	result, err := fixture.repository.(store.ExecutionResultStore).GetExecutionResult(fixture.ctx, fixture.accountID, fixture.appID, history[1].ExecutionID)
	recoveryAPIMust(t, err)
	var output struct {
		First struct {
			ID    int
			Title string
		}
		Second struct {
			ID    int
			Title string
		}
	}
	recoveryAPIMust(t, json.Unmarshal(result.Output, &output))
	// Both response identities and their nonempty provider text must survive the continuation.
	if output.First.ID != 1 || output.Second.ID != 2 || output.First.Title == "" || output.Second.Title == "" {
		t.Fatalf("unexpected resumed output: %s", result.Output)
	}
	t.Logf("mode=%s run=%s attempts=2 first_operation_calls=1 second_operation_calls=1 output=%s", mode, first.ExecutionID, result.Output)
}

// assertRecoveryLiveServiceCall checks encrypted replay evidence for exactly one successful physical call per attempt.
func assertRecoveryLiveServiceCall(t *testing.T, fixture mcpOperationPostgresFixture, executionID uuid.UUID, operation string, wantID int) {
	t.Helper()
	raw, err := fixture.repository.(store.ExecutionReplayEvidenceStore).GetReplayEvidence(fixture.ctx, fixture.accountID, fixture.appID, executionID, bytes.Repeat([]byte{7}, 32))
	recoveryAPIMust(t, err)
	var history capabilityReplayHistory
	recoveryAPIMust(t, json.Unmarshal(raw, &history))
	calls := 0
	// Snapshot host calls are recorded too; only named service operations count as provider work.
	for _, call := range history.Calls {
		var request struct {
			Service   string
			Operation string
		}
		recoveryAPIMust(t, json.Unmarshal(call.Request, &request))
		// Local snapshot traffic cannot be mistaken for a network operation.
		if request.Service == "" {
			continue
		}
		calls++
		var response struct {
			ID    int
			Title string
		}
		recoveryAPIMust(t, json.Unmarshal(call.Response, &response))
		// Exact operation, successful response and provider identity rule out substituted or repeated work.
		if request.Service != "todos" || request.Operation != operation || call.Error != "" || response.ID != wantID || response.Title == "" {
			t.Fatalf("unexpected provider evidence: request=%s response=%s error=%s", call.Request, call.Response, call.Error)
		}
	}
	// A resumed attempt must not dispatch the operation already represented by its snapshot.
	if calls != 1 {
		t.Fatalf("attempt %s provider calls=%d, want one %s", executionID, calls, operation)
	}
	t.Logf("execution=%s confirmed real HTTPS operation=%s todo_id=%d", executionID, operation, wantID)
}
