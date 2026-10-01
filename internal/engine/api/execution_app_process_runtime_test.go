package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

type processCompilerHost struct {
	data  json.RawMessage
	calls int
}

// Fetch verifies the generated SDK-style binding reaches the reviewed service and operation.
func (host *processCompilerHost) Fetch(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Service   string
		Operation string
		Input     struct{ Name string }
	}
	// The test fails if generated code loses its exact selected operation or input envelope.
	if json.Unmarshal(raw, &request) != nil || request.Service != "greeting" || request.Operation != "greet" {
		return nil, errors.New("unexpected generated binding")
	}
	host.calls++
	return json.Marshal(map[string]string{"greeting": "Hello " + request.Input.Name})
}

// DBGet returns only the current invocation's test document.
func (host *processCompilerHost) DBGet(context.Context) (json.RawMessage, error) {
	return host.data, nil
}

// DBSet preserves the JSON-only storage bridge used by generated code.
func (host *processCompilerHost) DBSet(_ context.Context, data json.RawMessage) error {
	host.data = append(json.RawMessage(nil), data...)
	return nil
}

// TestGeneratedUnifiedAppRunsInProcess covers real TS, Zod, selected bindings, storage and repeated guest reuse.
func TestGeneratedUnifiedAppRunsInProcess(t *testing.T) {
	// Integration requires the same OS confinement as production; unit interpreter tests run separately.
	if !sandbox.IsCapabilityWorkerAvailable(context.Background()) {
		t.Skip("isolated execution worker unavailable on this host")
	}
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	pins := []executionCompilerSelection{{Service: "greeting", Operation: "greet", ServiceID: uuid.NewString(), ServiceVersionID: uuid.NewString(), EndpointID: uuid.NewString()}}
	const source = `import * as z from "zod/mini";
 import {buildUnifiedApp} from "@fused/unified-app";
 import {fused} from "@fused/operations";
 export default buildUnifiedApp({input:z.object({name:z.string()}),output:z.object({greeting:z.string()}),
 // The operation binding and persistent document both use the Engine-owned host bridge.
 async execute({input}){const result=z.object({greeting:z.string()}).parse(await fused.greeting.greet({name:input.name}));await fused.db.set({name:input.name});return result;}});`
	artifact, err := runExecutionCompiler(context.Background(), source, pins)
	if err != nil {
		t.Fatal(err)
	}
	manager := sandbox.NewCapabilityWorkerManager()
	defer manager.Close()
	for _, name := range []string{"Ada", "Grace"} {
		assertProcessGeneratedRun(t, manager, []byte(artifact.BundleJS), name)
	}
	host := &processCompilerHost{}
	control, err := sandbox.NewCapabilityDeterminism()
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Run(context.Background(), "family", "v1", []byte(artifact.BundleJS), json.RawMessage(`{"name":42}`), host, control, true, nil)
	// Invalid caller input must fail Zod before any provider authority is exercised.
	if err == nil || host.calls != 0 {
		t.Fatalf("invalid input reached provider: %v, calls=%d", err, host.calls)
	}
}

// assertProcessGeneratedRun checks one identity's output and data without sharing its host with another request.
func assertProcessGeneratedRun(t *testing.T, manager *sandbox.CapabilityWorkerManager, bundle []byte, name string) {
	t.Helper()
	host := &processCompilerHost{}
	control, err := sandbox.NewCapabilityDeterminism()
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"name": name})
	output, err := manager.Run(context.Background(), "family", "v1", bundle, input, host, control, true, nil)
	// Correct TypeScript execution must preserve both the public output and separate stored data.
	if err != nil || string(output) != fmt.Sprintf(`{"greeting":"Hello %s"}`, name) || string(host.data) != string(input) {
		t.Fatalf("output=%s data=%s error=%v", output, host.data, err)
	}
}
