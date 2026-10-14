package executionappvm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

// configuredWorkerFixture drives the actual framed child loop with bounded test lifetime and explicit capacity.
func configuredWorkerFixture(t *testing.T, capacity int) (*json.Encoder, *bufio.Scanner) {
	t.Helper()
	input, parent := io.Pipe()
	output, child := io.Pipe()
	// Closing every endpoint releases blocked protocol writers even when an assertion fails.
	cleanup := func() { _ = parent.Close(); _ = input.Close(); _ = output.Close(); _ = child.Close() }
	timer := time.AfterFunc(10*time.Second, cleanup)
	t.Cleanup(func() { timer.Stop(); cleanup() })
	// Use the production child loop while avoiding unsupported OS confinement in portable protocol tests.
	go func() {
		runLoadedWorker(frameScanner(input), &frameWriter{encoder: json.NewEncoder(child)}, Frame{Kind: "load", Bundle: []byte(workerTestBundle), WorkerConcurrency: &capacity})
		_ = child.Close()
	}()
	scanner := frameScanner(output)
	ack := readWorkerTestFrame(t, scanner)
	// The parent must be able to verify that the child installed exactly the requested ceiling.
	if ack.Kind != "loaded" || ack.WorkerConcurrency == nil || *ack.WorkerConcurrency != capacity {
		t.Fatalf("capacity acknowledgment=%+v", ack)
	}
	return json.NewEncoder(parent), scanner
}

// TestLoadedWorkerConfiguredConcurrency proves more than four isolated realms overlap, including above the old 32-request occupancy cap.
func TestLoadedWorkerConfiguredConcurrency(t *testing.T) {
	for _, capacity := range []int{8, 40} {
		// Each capacity gets an independent worker so canceled requests cannot affect another run.
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			encoder, scanner := configuredWorkerFixture(t, capacity)
			control, err := NewDeterminism()
			// Authored execution still requires the normal deterministic context at higher capacity.
			if err != nil {
				t.Fatal(err)
			}
			calls := fillConfiguredWorker(t, encoder, scanner, control, capacity)
			// Cancel one request at full occupancy; its completion must release a slot without killing siblings.
			if err := encoder.Encode(Frame{Kind: "cancel", RequestID: 1}); err != nil {
				t.Fatal(err)
			}
			done := readWorkerTestFrame(t, scanner)
			// Cancellation uses the normal final-frame lifecycle, keeping parent and child slot accounting aligned.
			if done.Kind != "done" || done.RequestID != 1 || done.Error == "" {
				t.Fatalf("cancellation=%+v", done)
			}
			delete(calls, 1)
			next := uint64(capacity + 1)
			// The freed slot accepts a new interpreter while all other calls remain active.
			if err := encoder.Encode(Frame{Kind: "run", RequestID: next, Input: json.RawMessage(`{}`), Determinism: control}); err != nil {
				t.Fatal(err)
			}
			call := readWorkerTestFrame(t, scanner)
			// Reuse must preserve monotonically increasing request identity.
			if call.Kind != "call" || call.RequestID != next {
				t.Fatalf("replacement=%+v", call)
			}
			calls[next] = call
			completeConfiguredWorkerCalls(t, encoder, scanner, calls)
		})
	}
}

// TestLoadedWorkerRejectsCapacityOverflow preserves a hard child boundary even if a parent over-admits requests.
func TestLoadedWorkerRejectsCapacityOverflow(t *testing.T) {
	worker := &loadedWorker{capacity: 8, active: make(map[uint64]*loadedRequest), lastID: 8}
	for id := uint64(1); id <= 8; id++ {
		worker.active[id] = &loadedRequest{}
	}
	// Admission must fail before creating a ninth interpreter or touching uninitialized program state.
	if err := worker.start(Frame{Kind: "run", RequestID: 9}); err == nil {
		t.Fatal("child exceeded configured capacity")
	}
	for _, capacity := range []int{0, -1, 1025} {
		// Explicit invalid values must never be interpreted as a legacy default or unlimited capacity.
		if _, err := loadedWorkerCapacity(&capacity); err == nil {
			t.Fatalf("accepted %d", capacity)
		}
	}
}

// completeConfiguredWorkerCalls verifies distinct replies release all concurrently blocked interpreters.
func completeConfiguredWorkerCalls(t *testing.T, encoder *json.Encoder, scanner *bufio.Scanner, calls map[uint64]Frame) {
	t.Helper()
	for id, call := range calls {
		value := json.RawMessage(fmt.Sprintf(`{"id":%d}`, id))
		// Distinct host replies prove that concurrent interpreter state and correlations do not leak.
		if err := encoder.Encode(Frame{Kind: "reply", RequestID: id, ID: call.ID, Value: value}); err != nil {
			t.Fatal(err)
		}
		done := readWorkerTestFrame(t, scanner)
		// Completion must belong to this reply even when requests finish out of their starting order.
		if done.Kind != "done" || done.RequestID != id || done.Error != "" || string(done.Value) != string(value) {
			t.Fatalf("completion=%+v", done)
		}
	}
}

// fillConfiguredWorker holds the full requested capacity at host calls before allowing any completion.
func fillConfiguredWorker(t *testing.T, encoder *json.Encoder, scanner *bufio.Scanner, control Determinism, capacity int) map[uint64]Frame {
	t.Helper()
	calls := make(map[uint64]Frame)
	for id := uint64(1); id <= uint64(capacity); id++ {
		// Wait for each host call before starting the next; all remain blocked simultaneously.
		if err := encoder.Encode(Frame{Kind: "run", RequestID: id, Input: json.RawMessage(`{}`), Determinism: control}); err != nil {
			t.Fatal(err)
		}
		call := readWorkerTestFrame(t, scanner)
		// A premature done or shutdown exposes an unmodified child concurrency cap.
		if call.Kind != "call" || call.RequestID != id {
			t.Fatalf("unexpected call=%+v", call)
		}
		calls[id] = call
	}
	return calls
}
