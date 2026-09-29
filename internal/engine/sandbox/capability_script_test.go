package sandbox

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type capabilityScriptTestHost struct {
	mu       sync.Mutex
	data     json.RawMessage
	fetched  int
	ordinals []int
}

// Fetch records host calls and returns a bounded provider-shaped response.
func (host *capabilityScriptTestHost) Fetch(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Input struct {
			Value string `json:"value"`
		} `json:"input"`
	}
	// A malformed authored bridge request must fail before a response is forged.
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	host.mu.Lock()
	host.fetched++
	// The interpreter's ordinal records source call order even when host work completes concurrently.
	if ordinal, ok := CapabilityCallOrdinal(ctx); ok {
		host.ordinals = append(host.ordinals, ordinal)
	}
	host.mu.Unlock()
	return json.Marshal(map[string]string{"id": request.Input.Value})
}

// DBGet returns this execution's current document without sharing interpreter objects.
func (host *capabilityScriptTestHost) DBGet(context.Context) (json.RawMessage, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	return append(json.RawMessage(nil), host.data...), nil
}

// DBSet replaces only this execution's document, matching the public storage contract.
func (host *capabilityScriptTestHost) DBSet(_ context.Context, value json.RawMessage) error {
	host.mu.Lock()
	defer host.mu.Unlock()
	host.data = append(json.RawMessage(nil), value...)
	return nil
}

// TestRunCapabilityScriptExecutesAndStoresData covers the child interpreter independently of host OS isolation availability.
func TestRunCapabilityScriptExecutesAndStoresData(t *testing.T) {
	const bundle = `globalThis.FusedUnifiedApp = {
			input: { parse(value) { if (typeof value.name !== "string") throw Error("invalid"); return value; } },
			output: { parse(value) { if (typeof value.id !== "string") throw Error("invalid"); return value; } },
			async execute({input}) {
				const [first, second] = await Promise.all([
					__fusedHost.fetch(JSON.stringify({input:{value:input.name}})),
					__fusedHost.fetch(JSON.stringify({input:{value:input.name + "-2"}}))
				]);
				const id = JSON.parse(first).id;
				await __fusedHost.dbSet(JSON.stringify({id, second:JSON.parse(second).id}));
				return {id};
			}
	};`
	host := &capabilityScriptTestHost{}
	output, err := runCapabilityScriptInProcess(context.Background(), []byte(bundle), json.RawMessage(`{"name":"Jane"}`), host)
	// The public output must be exact and independent of stored execution data.
	if err != nil || string(output) != `{"id":"Jane"}` {
		t.Fatalf("RunCapabilityScript() = %s, %v", output, err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	// Both parallel provider calls must settle before the data write is visible.
	if host.fetched != 2 || string(host.data) != `{"id":"Jane","second":"Jane-2"}` {
		t.Fatalf("host calls = %d, data = %s", host.fetched, host.data)
	}
}

// TestRunCapabilityScriptRejectsInvalidOutput keeps a child-side Zod rejection out of public output.
func TestRunCapabilityScriptRejectsInvalidOutput(t *testing.T) {
	const bundle = `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(){throw Error("secret value")}},async execute(){return {secret:"hidden"}}};`
	_, err := runCapabilityScriptInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), &capabilityScriptTestHost{})
	// Validation detail may contain private authored values and must stay bounded.
	if err == nil || err.Error() != "capability execution failed" {
		t.Fatalf("invalid output error = %v", err)
	}
}

// TestRunCapabilityScriptBoundsData verifies the child interpreter rejects an oversized write before storage.
func TestRunCapabilityScriptBoundsData(t *testing.T) {
	const bundle = `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},async execute(){await __fusedHost.dbSet(JSON.stringify({value:"x".repeat(524288)}));return {ok:true}}};`
	host := &capabilityScriptTestHost{}
	_, err := runCapabilityScriptInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), host)
	// Runtime rejects the payload before delegating to the durable store.
	if err == nil {
		t.Fatal("oversized document should fail")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	// A rejected write must leave this execution's document untouched.
	if len(host.data) != 0 {
		t.Fatal("oversized document reached storage")
	}
}

// TestCapabilityDeterminismPinsTimeAndRandom proves replay controls produce identical JavaScript output.
func TestCapabilityDeterminismPinsTimeAndRandom(t *testing.T) {
	const bundle = `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},async execute(){return {now:Date.now(),constructed:new Date().getTime(),random:[Math.random(),Math.random()]}}};`
	control := CapabilityDeterminism{StartedAtUnixMs: 1700000000123, RandomSeed: 42}
	host := &capabilityScriptTestHost{}
	first, err := runCapabilityScriptInProcessWithDeterminism(context.Background(), []byte(bundle), json.RawMessage(`{}`), host, control)
	if err != nil {
		t.Fatalf("first execution: %v", err)
	}
	second, err := runCapabilityScriptInProcessWithDeterminism(context.Background(), []byte(bundle), json.RawMessage(`{}`), host, control)
	if err != nil || string(second) != string(first) {
		t.Fatalf("replay output changed: first=%s second=%s error=%v", first, second, err)
	}
	assertDeterministicCapabilityOutput(t, first, control)
	control.RandomSeed++
	third, err := runCapabilityScriptInProcessWithDeterminism(context.Background(), []byte(bundle), json.RawMessage(`{}`), host, control)
	if err != nil || string(third) == string(first) {
		t.Fatalf("different seed did not change output: third=%s error=%v", third, err)
	}
}

// assertDeterministicCapabilityOutput verifies both fixed time and nonconstant random draws.
func assertDeterministicCapabilityOutput(t *testing.T, raw json.RawMessage, control CapabilityDeterminism) {
	t.Helper()
	var result struct {
		Now         int64     `json:"now"`
		Constructed int64     `json:"constructed"`
		Random      []float64 `json:"random"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Now != control.StartedAtUnixMs || result.Constructed != control.StartedAtUnixMs || len(result.Random) != 2 || result.Random[0] == result.Random[1] {
		t.Fatalf("uncontrolled runtime output: %+v", result)
	}
}

// TestCapabilityDeterminismRejectsMissingControls fails before an unaudited worker can run.
func TestCapabilityDeterminismRejectsMissingControls(t *testing.T) {
	_, err := RunCapabilityScriptWithDeterminism(context.Background(), []byte(`x`), json.RawMessage(`{}`), &capabilityScriptTestHost{}, CapabilityDeterminism{})
	if err == nil || err.Error() != "capability deterministic controls are invalid" {
		t.Fatalf("invalid controls error = %v", err)
	}
}
