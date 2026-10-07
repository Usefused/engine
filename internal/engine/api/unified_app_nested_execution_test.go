package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// nestedAppArtifact pins the built test compiler and exercises the production Zod boundary without dummy physical selections.
func nestedAppArtifact(t *testing.T, body string) *executionPlanArtifact {
	t.Helper()
	// Go tests run from this package directory, not the repository root used by the development compiler fallback.
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	source := `import * as z from "zod/mini";
import {buildUnifiedApp, fused} from "@fused/unified-app";
export default buildUnifiedApp({input:z.object({value:z.string()}),output:z.object({value:z.string()}),
// Each test body uses only the admitted Engine bridge.
async execute({input}){` + body + `}});`
	artifact, err := runExecutionCompiler(context.Background(), source, []executionCompilerSelection{})
	// No worker should be started with unchecked or stale source.
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

// newNestedAppServer pins one real compiled child and retains both parent and child execution records.
func newNestedAppServer(t *testing.T) (*EngineGRPCServer, *attachedExecutionFixture, auth.RuntimeIdentity) {
	t.Helper()
	server, targetID := newRESTPhysicalServer(&restRuntimeTestDouble{})
	base := server.store.(*grpcRuntimeStore)
	child := nestedAppArtifact(t, `await fused.db.set({owner:"child"});return {value:input.value+" child"};`)
	family := uuid.New()
	bundle := store.UnifiedAppBundle{AppID: targetID, SourceHash: "sha256:child", BundleJS: child.BundleJS, Manifest: child.Manifest}
	fixture := &attachedExecutionFixture{
		capabilityRouteStore: &capabilityRouteStore{Store: base, bundle: bundle, activeAppID: targetID},
		target:               store.AppRuntime{AppID: targetID, AccountID: base.accountID, AppFamilyID: family, Kind: store.AppKindUnifiedApp, Status: store.AppStatusActive, Version: "1.0.0", BundleDigest: child.Digest},
		binding:              models.UnifiedAppBinding{Alias: "child", AppID: targetID, AppFamilyID: family, SourceHash: bundle.SourceHash, BundleDigest: child.Digest},
	}
	server.store = fixture
	// Two interpreter slots allow a synchronous child while retaining the waiting parent.
	if err := server.ConfigureUnifiedAppAdmission(config.UnifiedAppAdmissionConfig{MaxConcurrency: 2, QueueCapacity: 16, QueueTimeoutSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.capabilityWorkerManager().Close)
	identity := auth.RuntimeIdentity{AccountID: base.accountID, AppFamilyID: uuid.New(), AppID: uuid.New(), TokenID: uuid.New(), Kind: store.AppKindUnifiedApp, AppVersion: "1.0.0", Status: store.AppStatusActive, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	return server, fixture, identity
}

// TestUnifiedAppNestedCompiledExecution runs a real parent-to-child call and checks output, attribution and data isolation.
func TestUnifiedAppNestedCompiledExecution(t *testing.T) {
	requireCompileIsolation(t)
	server, fixture, identity := newNestedAppServer(t)
	parent := nestedAppArtifact(t, `const result=await fused.callApp("child",input);
// Failed children must not be presented as successful parent output.
if(result.status!=="succeeded")throw Error("child failed");
await fused.db.set({owner:"parent"});return z.object({value:z.string()}).parse(result.output);`)
	manifest, err := parseUnifiedAppManifest(parent.Manifest)
	// Compilation must produce the descriptor consumed by the durable executor.
	if err != nil {
		t.Fatal(err)
	}
	result, requestErr := server.executeCapabilityRun(context.Background(), capabilityRunSpec{
		identity: identity, version: "1.0.0", mode: "live", input: json.RawMessage(`{"value":"hello"}`), manifest: manifest,
		bundle: &store.UnifiedAppBundle{AppID: identity.AppID, SourceHash: "sha256:parent", BundleJS: parent.BundleJS, Manifest: parent.Manifest},
	})
	// The compiled helper must return the child's validated output through the parent's own schema.
	if requestErr != nil || result.Status != "succeeded" || string(result.Output) != `{"value":"hello child"}` {
		t.Fatalf("result=%+v error=%v", result, requestErr)
	}
	// Both executions are durable and preserve the original caller's token attribution.
	if len(fixture.records) != 2 {
		t.Fatalf("records=%d", len(fixture.records))
	}
	assertNestedAppRecords(t, fixture, identity.TokenID)
}

// assertNestedAppRecords checks independent persistence while retaining the originating token identity.
func assertNestedAppRecords(t *testing.T, fixture *attachedExecutionFixture, tokenID uuid.UUID) {
	t.Helper()
	for _, record := range fixture.records {
		// Delegation must not invent a child execution token or combine execution-local documents.
		if record.AppTokenID != tokenID || record.Status != "succeeded" {
			t.Fatalf("record=%+v", record)
		}
		want := `{"owner":"parent"}`
		// The child owns only its own data document and result row.
		if record.AppID == fixture.target.AppID {
			want = `{"owner":"child"}`
		}
		if string(record.Data) != want {
			t.Fatalf("data=%s want=%s", record.Data, want)
		}
	}
}

// TestUnifiedAppNestedReplay uses recorded child results without performing the child's effects again.
func TestUnifiedAppNestedReplay(t *testing.T) {
	requireCompileIsolation(t)
	server, fixture, identity := newNestedAppServer(t)
	host := &executionCapabilityHost{apps: server, identity: identity}
	recorder := newReplayTestRecorder(t, NewBufferedCapabilityHost(host))
	request := json.RawMessage(`{"unifiedApp":"child","input":{"value":"recorded"}}`)
	response, history := recordReplayFixture(t, context.Background(), recorder, request)
	replay, err := NewReplayCapabilityHost(history)
	// Hosted calls use the same authenticated transcript format as provider calls.
	if err != nil {
		t.Fatal(err)
	}
	fixture.activeAppID = uuid.New()
	replayed, err := replay.Fetch(context.Background(), request)
	// Promotion after recording cannot cause replay to contact the child or fail its live traffic check.
	if err != nil || string(replayed) != string(response) || len(fixture.records) != 1 {
		t.Fatalf("replay=%s err=%v records=%d", replayed, err, len(fixture.records))
	}
	// Live execution still enforces promotion rather than borrowing replay's recorded result.
	if _, err := host.Fetch(context.Background(), request); err == nil {
		t.Fatal("promoted child executed")
	}
}
