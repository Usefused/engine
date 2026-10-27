package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// TestAutomaticSnapshotProviderRecovery proves ordinary JS skips completed writes after replacing the worker and Engine runtime.
func TestAutomaticSnapshotProviderRecovery(t *testing.T) {
	fixture := openMCPAppOperationPostgresFixture(t)
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	var writes, reads atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The first integration writes once, while the following read fails on its first attempt.
		if r.URL.Path == "/write" {
			writes.Add(1)
			// Automatic keys must reach the actual provider as well as the Engine cache.
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("provider received no idempotency key")
			}
			_, _ = w.Write([]byte(`{"id":1}`))
			return
		}
		// A retryable read forces a replacement to replay the already committed write.
		if reads.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"try again"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":2}`))
	}))
	defer vendor.Close()
	source := `import * as z from "zod/mini";
import {buildUnifiedApp} from "@fused/unified-app";
import {fused} from "@fused/operations";
export default buildUnifiedApp({snapshot:true,recovery:{mode:"manual",maxAttempts:3,backoffSeconds:1},
input:z.object({}),output:z.object({first:z.number(),second:z.number(),stamp:z.number(),random:z.number()}),
// Normal authored control flow needs neither manual saves nor a resume cursor.
async execute(){const stamp=Date.now();const random=Math.random();
const first=z.object({id:z.number()}).parse(await fused.todos.first({}, {idempotencyKey:"shared-order"}));const second=z.object({id:z.number()}).parse(await fused.todos.second({}));
return {first:first.id,second:second.id,stamp,random};}});`
	pins := []executionCompilerSelection{
		{Service: "todos", Operation: "first", ServiceID: fixture.serviceID.String(), ServiceVersionID: fixture.serviceVersionID.String(), EndpointID: fixture.listEndpointID.String()},
		{Service: "todos", Operation: "second", ServiceID: fixture.serviceID.String(), ServiceVersionID: fixture.serviceVersionID.String(), EndpointID: fixture.createEndpointID.String()},
	}
	artifact, err := runExecutionCompiler(fixture.ctx, source, pins)
	recoveryAPIMust(t, err)
	seedRecoveryApp(t, fixture, artifact)
	seedRecoveryLiveService(t, fixture)
	snapshot := store.ServiceContractSnapshot{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(), ServiceID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Version: "live-test",
		ServiceMetadata: fusedobject.ServiceMetadata{ID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Name: "Snapshot fixture", BaseURL: vendor.URL},
		Endpoints: []fusedobject.Endpoint{
			{ID: fixture.listEndpointID, Name: "first", Method: "POST", Path: "/write", NormalizedPath: "/write", SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}},
			{ID: fixture.createEndpointID, Name: "second", Method: "GET", Path: "/read", NormalizedPath: "/read", SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}},
		}}
	_, err = fixture.snapshotStore.UpsertServiceContractSnapshot(fixture.ctx, snapshot)
	recoveryAPIMust(t, err)
	sandbox.SetIdempotencyStore(fixture.repository)
	t.Cleanup(func() { sandbox.SetIdempotencyStore(nil) })
	server := newRecoveryLiveServiceServer(t, fixture)
	first := initialRecoveryLiveServiceRun(t, fixture, server, "webhook")
	// The interrupted attempt must have reached both provider boundaries exactly once.
	if first.Status == "succeeded" || writes.Load()+reads.Load() != 2 {
		t.Fatalf("first=%+v writes=%d reads=%d", first, writes.Load(), reads.Load())
	}
	server.capabilityWorkerManager().Close()
	replacement := newRecoveryLiveServiceServer(t, fixture)
	sandbox.SetIdempotencyStore(store.NewPostgresStore(fixture.pool))
	triggerManualRecovery(t, fixture, first.ExecutionID)
	replacement.runExecutionRecoveryBatch(fixture.ctx)
	history, err := fixture.repository.(store.ExecutionRecoveryStore).ListExecutionAttempts(fixture.ctx, fixture.accountID, fixture.appID, first.ExecutionID)
	recoveryAPIMust(t, err)
	// Durable replay must work with a new worker, new store wrapper, and a new attempt ID.
	if len(history) != 2 || history[1].Status != "succeeded" || writes.Load() != 1 || reads.Load() != 2 {
		t.Fatalf("history=%+v writes=%d reads=%d", history, writes.Load(), reads.Load())
	}
	assertAutomaticSnapshotResult(t, fixture, first.ExecutionID, history[1].ExecutionID)
	assertAutomaticSnapshotControls(t, fixture, first.ExecutionID, history[1].ExecutionID)
	assertSnapshotSharedKeyRun(t, fixture, replacement, &writes, &reads)
}

// assertAutomaticSnapshotResult checks the final app output and shared provider rows after a replacement succeeds.
func assertAutomaticSnapshotResult(t *testing.T, fixture mcpOperationPostgresFixture, runID, finalID uuid.UUID) {
	t.Helper()
	final, err := fixture.repository.(store.ExecutionResultStore).GetExecutionResult(fixture.ctx, fixture.accountID, fixture.appID, finalID)
	recoveryAPIMust(t, err)
	var output struct {
		First, Second int
		Stamp         int64
		Random        float64
	}
	recoveryAPIMust(t, json.Unmarshal(final.Output, &output))
	// Cached bytes must be indistinguishable from the original successful provider response.
	if output.First != 1 || output.Second != 2 || output.Stamp == 0 {
		t.Fatalf("output=%s", final.Output)
	}
	var count int
	recoveryAPIMust(t, fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM fused_engine_idempotency_keys WHERE app_id=$1 AND run_id=$2 AND NOT pending`, fixture.appID, runID).Scan(&count))
	// The same two response rows serve both attempts; no per-attempt response journal is added.
	if count != 2 {
		t.Fatalf("completed provider responses=%d", count)
	}
	t.Logf("run=%s attempts=2 provider writes=1 read attempts=2", runID)
}

// TestAutomaticSnapshotKeys isolates opt-in, explicit override, and stable call identity from provider transport.
func TestAutomaticSnapshotKeys(t *testing.T) {
	run := uuid.New()
	host := &executionCapabilityHost{snapshotRunID: run}
	key, err := host.providerIdempotencyKey(capabilityHostFetchRequest{}, 1)
	recoveryAPIMust(t, err)
	// Snapshot-disabled apps must never receive synthetic provider keys.
	if key != "" {
		t.Fatal("disabled snapshot generated a key")
	}
	host.automaticSnapshot = true
	first, err := host.providerIdempotencyKey(capabilityHostFetchRequest{}, 1)
	recoveryAPIMust(t, err)
	host.executionID = uuid.New()
	repeated, err := host.providerIdempotencyKey(capabilityHostFetchRequest{}, 1)
	recoveryAPIMust(t, err)
	second, err := host.providerIdempotencyKey(capabilityHostFetchRequest{}, 2)
	recoveryAPIMust(t, err)
	// Attempt replacement preserves identity; identical intentional calls keep distinct ordinals.
	if first != repeated || first == second || first != fmt.Sprintf("fused:run:%s:call:1", run) {
		t.Fatalf("keys=%q %q %q", first, repeated, second)
	}
	explicit, err := host.providerIdempotencyKey(capabilityHostFetchRequest{IdempotencyKey: "order-123"}, 1)
	recoveryAPIMust(t, err)
	// The author's business key takes precedence over generated identity.
	if explicit != "order-123" {
		t.Fatal("explicit key overwritten")
	}
}

// assertAutomaticSnapshotControls proves replacement workers keep the original clock and random stream.
func assertAutomaticSnapshotControls(t *testing.T, fixture mcpOperationPostgresFixture, first, second uuid.UUID) {
	t.Helper()
	repository := fixture.repository.(store.ExecutionReplayEvidenceStore)
	var histories [2]capabilityReplayHistory
	for index, id := range []uuid.UUID{first, second} {
		raw, err := repository.GetReplayEvidence(fixture.ctx, fixture.accountID, fixture.appID, id, bytes.Repeat([]byte{7}, 32))
		recoveryAPIMust(t, err)
		recoveryAPIMust(t, json.Unmarshal(raw, &histories[index]))
	}
	// Replaying calls with a fresh clock or seed could generate different provider inputs and business keys.
	if histories[0].Determinism != histories[1].Determinism || !histories[0].Determinism.Valid() {
		t.Fatalf("controls changed: %+v %+v", histories[0].Determinism, histories[1].Determinism)
	}
}

// TestAutomaticSnapshotRejectsUncheckpointedEffects prevents implicit fallback to manual state or nested app writes.
func TestAutomaticSnapshotRejectsUncheckpointedEffects(t *testing.T) {
	host := &executionCapabilityHost{automaticSnapshot: true}
	for _, request := range []string{`{"snapshot":"save","state":{}}`, `{"snapshot":"load"}`, `{"unifiedApp":"child","input":{}}`} {
		_, err := host.Fetch(context.Background(), json.RawMessage(request))
		// Unsupported effects must fail before accessing persistence or another app runtime.
		if err == nil {
			t.Fatalf("unsupported effect accepted: %s", request)
		}
	}
}

// assertSnapshotSharedKeyRun exercises authored-key reuse across independent logical runs without copying provider bytes.
func assertSnapshotSharedKeyRun(t *testing.T, fixture mcpOperationPostgresFixture, server *EngineGRPCServer, writes, reads *atomic.Int32) {
	t.Helper()
	next := initialRecoveryLiveServiceRun(t, fixture, server, "webhook")
	// A new run reuses the explicit business key but receives a fresh generated key for its read.
	if next.Status != "succeeded" || writes.Load() != 1 || reads.Load() != 3 {
		t.Fatalf("shared key run=%+v writes=%d reads=%d", next, writes.Load(), reads.Load())
	}
	var references int
	recoveryAPIMust(t, fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM fused_engine_idempotency_runs WHERE run_id=$1`, next.ExecutionID).Scan(&references))
	// The replay must record its dependency before the new run consumes the shared response.
	if references != 1 {
		t.Fatalf("shared retention references=%d", references)
	}
}
