package sandbox

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCapabilityWorkerRejectsDuplicateHostCalls ensures a compromised child cannot repeat an effect or replay ordinal.
func TestCapabilityWorkerRejectsDuplicateHostCalls(t *testing.T) {
	ids := make(map[uint64]struct{})
	ordinals := make(map[int]struct{})
	first := capabilityProcessFrame{Kind: "call", ID: 1, Ordinal: 1}
	if err := admitCapabilityWorkerCall(first, ids, ordinals); err != nil {
		t.Fatal(err)
	}
	// Repeating either identifier would create a second provider effect for one authored call.
	if admitCapabilityWorkerCall(first, ids, ordinals) == nil {
		t.Fatal("duplicate call ID was admitted")
	}
	if admitCapabilityWorkerCall(capabilityProcessFrame{Kind: "call", ID: 2, Ordinal: 1}, ids, ordinals) == nil {
		t.Fatal("duplicate call ordinal was admitted")
	}
}

// TestCapabilityWorkerPreservesInterpreterOrdinal verifies parent host context uses authored call order.
func TestCapabilityWorkerPreservesInterpreterOrdinal(t *testing.T) {
	host := &capabilityScriptTestHost{}
	request := capabilityProcessFrame{Kind: "call", ID: 7, Ordinal: 3, Method: "fetch", Payload: json.RawMessage(`{"input":{"value":"Jane"}}`)}
	value, err := dispatchCapabilityHostCall(context.Background(), host, request)
	if err != nil || string(value) != `{"id":"Jane"}` {
		t.Fatalf("host response = %s, %v", value, err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.ordinals) != 1 || host.ordinals[0] != 3 {
		t.Fatalf("host ordinals = %v", host.ordinals)
	}
}

// TestCapabilityWorkerRejectsUnselectedEffect ensures IPC never invents a new host operation.
func TestCapabilityWorkerRejectsUnselectedEffect(t *testing.T) {
	request := capabilityProcessFrame{Kind: "call", ID: 1, Ordinal: 1, Method: "http", Payload: json.RawMessage(`{}`)}
	_, err := dispatchCapabilityHostCall(context.Background(), &capabilityScriptTestHost{}, request)
	if err == nil {
		t.Fatal("unknown host operation was admitted")
	}
}

// TestCapabilityWorkerRoundTripsDeterminism verifies IPC preserves the parent clock and seed.
func TestCapabilityWorkerRoundTripsDeterminism(t *testing.T) {
	// Some test hosts cannot install production process isolation, so only the real worker path is exercised when available.
	if !IsCapabilityWorkerAvailable(context.Background()) {
		t.Skip("capability worker isolation is unavailable")
	}
	const bundle = `globalThis.FusedExecutionApp={input:{parse(v){return v}},output:{parse(v){return v}},async execute(){return {time:Date.now(),random:Math.random()}}};`
	control := CapabilityDeterminism{StartedAtUnixMs: 1700000000123, RandomSeed: 42}
	host := &capabilityScriptTestHost{}
	first, err := RunCapabilityScriptWithDeterminism(context.Background(), []byte(bundle), json.RawMessage(`{}`), host, control)
	if err != nil {
		t.Fatalf("first worker execution: %v", err)
	}
	second, err := RunCapabilityScriptWithDeterminism(context.Background(), []byte(bundle), json.RawMessage(`{}`), host, control)
	if err != nil || string(first) != string(second) {
		t.Fatalf("worker replay differs: first=%s second=%s error=%v", first, second, err)
	}
}
