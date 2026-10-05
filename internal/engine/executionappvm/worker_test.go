package executionappvm

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const workerTestBundle = `
globalThis.FusedExecutionManifest = {schemaVersion:1,inputSchema:{},outputSchema:{},searchable:[],selectedOperations:[]};
globalThis.FusedUnifiedApp = {
  input: {parse(value) {return value}},
  output: {parse(value) {return value}},
  execute: async ({input}) => JSON.parse(await __fusedHost.fetch(JSON.stringify(input)))
};`

// TestLoadedWorkerPreservesOperationMessage exercises the production framed bridge without OS isolation dependencies.
func TestLoadedWorkerPreservesOperationMessage(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	defer inputWriter.Close()
	defer outputReader.Close()
	// The real resident-worker loop ensures serialization cannot silently erase the typed explanation.
	go func() {
		runLoadedWorker(frameScanner(inputReader), &frameWriter{encoder: json.NewEncoder(outputWriter)}, Frame{Kind: "load", Bundle: []byte(workerTestBundle)})
		_ = outputWriter.Close()
	}()
	scanner := frameScanner(outputReader)
	readWorkerTestFrame(t, scanner)
	encoder := json.NewEncoder(inputWriter)
	control, err := NewDeterminism()
	// A deterministic run uses the same admission path as a deployed app.
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Frame{Kind: "run", RequestID: 1, Input: json.RawMessage(`{}`), Determinism: control}); err != nil {
		t.Fatal(err)
	}
	call := readWorkerTestFrame(t, scanner)
	// The test must observe a real awaited host call, rather than a bundle initialization failure.
	if call.Kind != "call" {
		t.Fatalf("expected host call: %#v", call)
	}
	if err := encoder.Encode(Frame{Kind: "reply", RequestID: 1, ID: call.ID, Error: "capability workspace operation failed", ErrorMessage: "No such price: price_example"}); err != nil {
		t.Fatal(err)
	}
	done := readWorkerTestFrame(t, scanner)
	// The worker must return the authored exception with its useful message and a safe implicit Error string.
	if done.Kind != "done" || done.Diagnostic == nil || !strings.Contains(done.Diagnostic.Message, "No such price: price_example") || done.Error != "capability execution failed" {
		t.Fatalf("message lost in worker IPC: %#v", done)
	}
}

// TestLoadedWorkerRoutesConcurrentReplies ensures overlapping calls with the same local ID stay isolated.
func TestLoadedWorkerRoutesConcurrentReplies(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	workerDone := make(chan int, 1)
	go func() {
		workerDone <- runLoadedWorker(frameScanner(inputReader), &frameWriter{encoder: json.NewEncoder(outputWriter)}, Frame{Kind: "load", Bundle: []byte(workerTestBundle)})
		_ = outputWriter.Close()
	}()
	defer inputWriter.Close()
	defer outputReader.Close()
	encoder := json.NewEncoder(inputWriter)
	scanner := frameScanner(outputReader)
	loaded := readWorkerTestFrame(t, scanner)
	// Only an acknowledged, validated bundle may accept execution requests.
	if loaded.Kind != "loaded" {
		t.Fatalf("expected loaded acknowledgment, got %q", loaded.Kind)
	}
	control, err := NewDeterminism()
	if err != nil {
		t.Fatal(err)
	}
	sendWorkerTestRuns(t, encoder, control)
	calls := collectWorkerTestCalls(t, scanner)
	sendWorkerTestReplies(t, encoder, calls)
	outputs := collectWorkerTestOutputs(t, scanner)
	// Matching values after reversed replies prove that request IDs isolate interpreters.
	if outputs[1] != `{"name":"Ada"}` || outputs[2] != `{"name":"Grace"}` {
		t.Fatalf("results crossed invocations: %#v", outputs)
	}
	_ = inputWriter.Close()
	// Graceful parent retirement should stop the resident process after all requests finish.
	if code := <-workerDone; code != 0 {
		t.Fatalf("worker exit code = %d", code)
	}
}

// sendWorkerTestRuns starts two interpreters in one loaded process.
func sendWorkerTestRuns(t *testing.T, encoder *json.Encoder, control Determinism) {
	t.Helper()
	for id, name := range []string{"Ada", "Grace"} {
		input, _ := json.Marshal(map[string]string{"name": name})
		// Distinct request IDs share the worker but each receives a fresh interpreter.
		if err := encoder.Encode(Frame{Kind: "run", RequestID: uint64(id + 1), Input: input, Determinism: control}); err != nil {
			t.Fatal(err)
		}
	}

}

// collectWorkerTestCalls waits for both host calls before allowing either response.
func collectWorkerTestCalls(t *testing.T, scanner *bufio.Scanner) map[uint64]Frame {
	t.Helper()
	calls := make(map[uint64]Frame)
	for len(calls) < 2 {
		frame := readWorkerTestFrame(t, scanner)
		// Both interpreters must reach the host before either host result is sent.
		if frame.Kind != "call" || frame.ID != 1 {
			t.Fatalf("expected independent first host calls, got %#v", frame)
		}
		calls[frame.RequestID] = frame
	}
	return calls
}

// sendWorkerTestReplies reverses provider completion order to exercise request correlation.
func sendWorkerTestReplies(t *testing.T, encoder *json.Encoder, calls map[uint64]Frame) {
	t.Helper()
	for _, id := range []uint64{2, 1} {
		name := "Ada"
		// Reversing replies proves correlation does not rely on frame arrival order.
		if id == 2 {
			name = "Grace"
		}
		value, _ := json.Marshal(map[string]string{"name": name})
		if err := encoder.Encode(Frame{Kind: "reply", RequestID: id, ID: calls[id].ID, Value: value}); err != nil {
			t.Fatal(err)
		}
	}

}

// collectWorkerTestOutputs requires one successful result per request ID.
func collectWorkerTestOutputs(t *testing.T, scanner *bufio.Scanner) map[uint64]string {
	t.Helper()
	outputs := make(map[uint64]string)
	for len(outputs) < 2 {
		frame := readWorkerTestFrame(t, scanner)
		// Every result must retain its invocation ID and the matching provider value.
		if frame.Kind != "done" || frame.Error != "" {
			t.Fatalf("expected successful result, got %#v", frame)
		}
		// Each concurrent interpreter must return its own complete bounded phase evidence over IPC.
		if len(frame.Phases) != 5 || !ValidExecutionPhases(frame.Phases) {
			t.Fatalf("missing phase metadata: %#v", frame.Phases)
		}
		outputs[frame.RequestID] = string(frame.Value)
	}
	return outputs
}

// TestLoadedWorkerRejectsReload ensures one process cannot silently change its app version.
func TestLoadedWorkerRejectsReload(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	workerDone := make(chan int, 1)
	go func() {
		workerDone <- runLoadedWorker(frameScanner(inputReader), &frameWriter{encoder: json.NewEncoder(outputWriter)}, Frame{Kind: "load", Bundle: []byte(workerTestBundle)})
		_ = outputWriter.Close()
	}()
	defer inputWriter.Close()
	defer outputReader.Close()
	scanner := frameScanner(outputReader)
	_ = readWorkerTestFrame(t, scanner)
	// A later load is a protocol violation even if it happens to repeat the same source.
	if err := json.NewEncoder(inputWriter).Encode(Frame{Kind: "load", Bundle: []byte(workerTestBundle)}); err != nil {
		t.Fatal(err)
	}
	if code := <-workerDone; code != 1 {
		t.Fatalf("reload exit code = %d", code)
	}
}

// readWorkerTestFrame decodes one bounded worker frame for protocol assertions.
func readWorkerTestFrame(t *testing.T, scanner *bufio.Scanner) Frame {
	t.Helper()
	// A missing frame indicates a worker exit or broken test pipe, never a valid result.
	if !scanner.Scan() {
		t.Fatalf("missing worker frame: %v", scanner.Err())
	}
	frame, err := decodeFrame(scanner.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
