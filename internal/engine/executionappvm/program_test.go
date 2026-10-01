package executionappvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const isolatedProgramBundle = `
globalThis.FusedExecutionManifest = {schemaVersion:1};
let calls = 0;
globalThis.FusedUnifiedApp = {
  input: {parse(v) { return v; }}, output: {parse(v) { return v; }},
  execute: async ({input}) => {
    // A reused runtime would leak both module state and prototype mutations.
    if (++calls !== 1 || Object.prototype.previousCaller) throw new Error("state leaked");
    Object.prototype.previousCaller = input.name;
    // Cancellation must not poison the shared program or a subsequent runtime.
    if (input.spin) { while (true) {} }
    return {name: input.name, now: Date.now(), random: Math.random()};
  }
};`

// TestCachedProgramKeepsRuntimeStateIsolated shares bytecode across sequential and concurrent callers.
func TestCachedProgramKeepsRuntimeStateIsolated(t *testing.T) {
	program, err := loadCapabilityProgram(Frame{Bundle: []byte(isolatedProgramBundle)})
	// Inspection and execution must use the same immutable program without retaining inspection globals.
	if err != nil {
		t.Fatal(err)
	}
	control := Determinism{StartedAtUnixMs: 1700000000123, RandomSeed: 42}
	for index := range 16 {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			t.Parallel()
			input := json.RawMessage(fmt.Sprintf(`{"name":"caller-%d"}`, index))
			var previous string
			for attempt := range 2 {
				host := &phaseTestHost{}
				output, err := runInProcessWithProgram(context.Background(), nil, program, input, host, control)
				// A nil source proves execution uses the cache, while every runtime must start with clean state.
				if err != nil || !strings.Contains(string(output), fmt.Sprintf(`"name":"caller-%d"`, index)) {
					t.Fatalf("output=%s error=%v", output, err)
				}
				// Replaying identical controls must survive cached code and fresh runtime initialization.
				if attempt > 0 && previous != string(output) {
					t.Fatalf("determinism changed: %s != %s", previous, output)
				}
				previous = string(output)
				assertCachedProgramPhases(t, host.phases)
			}
		})
	}
}

// TestCachedProgramSurvivesCancellation ensures interruption belongs to the disposable runtime, not bytecode.
func TestCachedProgramSurvivesCancellation(t *testing.T) {
	program, err := compileCapabilityBundle([]byte(isolatedProgramBundle))
	// Compilation failure would invalidate the cancellation fixture itself.
	if err != nil {
		t.Fatal(err)
	}
	control := Determinism{StartedAtUnixMs: 1700000000123, RandomSeed: 42}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = runInProcessWithProgram(ctx, nil, program, json.RawMessage(`{"spin":true}`), diagnosticTestHost{}, control)
	// An infinite loop must terminate at the invocation deadline.
	if err == nil || ctx.Err() == nil {
		t.Fatalf("loop escaped cancellation: %v", err)
	}
	output, err := runInProcessWithProgram(context.Background(), nil, program, json.RawMessage(`{"name":"next"}`), diagnosticTestHost{}, control)
	// The next request must receive neither the interrupt nor the previous prototype mutation.
	if err != nil || !strings.Contains(string(output), `"name":"next"`) {
		t.Fatalf("cached program poisoned: output=%s error=%v", output, err)
	}
}

// TestCachedProgramRetainsMappedDiagnostics keeps source maps with bytecode after original bytes are released.
func TestCachedProgramRetainsMappedDiagnostics(t *testing.T) {
	sourceMap := base64.StdEncoding.EncodeToString([]byte(`{"version":3,"sources":["unified-app.ts"],"names":[],"mappings":"AAiBA;AAAA"}`))
	source := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{throw new Error("private mapped error")}};`
	program, err := compileCapabilityBundle([]byte(source + "\n//# sourceMappingURL=data:application/json;base64," + sourceMap))
	// The mapping fixture must compile under the same inline-only policy as resident workers.
	if err != nil {
		t.Fatal(err)
	}
	control := Determinism{StartedAtUnixMs: 1700000000123, RandomSeed: 42}
	_, err = runInProcessWithProgram(context.Background(), nil, program, json.RawMessage(`{}`), diagnosticTestHost{}, control)
	detail := PrivateDiagnostic(err)
	// Private mapped frames remain available without leaking authored exception text into the public error.
	if detail == nil || !strings.Contains(detail.Stack, "unified-app.ts:18") || strings.Contains(err.Error(), "private") {
		t.Fatalf("unexpected cached diagnostic: %#v, %v", detail, err)
	}
}

// TestLoadCapabilityProgramRejectsInvalidSource prevents a failed compile from entering the resident cache.
func TestLoadCapabilityProgramRejectsInvalidSource(t *testing.T) {
	for _, source := range []string{"", "const =", "globalThis.FusedUnifiedApp = {}", strings.Repeat(" ", MaxBundleBytes+1)} {
		_, err := loadCapabilityProgram(Frame{Bundle: []byte(source)})
		// Invalid size, syntax and declarations must all fail before the loaded acknowledgment.
		if err == nil {
			t.Fatalf("accepted invalid bundle of %d bytes", len(source))
		}
	}
}

// TestCachedProgramValidatesEveryInvocation prevents a successful load from authorizing malformed requests.
func TestCachedProgramValidatesEveryInvocation(t *testing.T) {
	program, err := compileCapabilityBundle([]byte(isolatedProgramBundle))
	// The fixture must load before testing request-level checks on the cache path.
	if err != nil {
		t.Fatal(err)
	}
	control := Determinism{StartedAtUnixMs: 1700000000123, RandomSeed: 42}
	tests := []struct {
		name    string
		input   json.RawMessage
		host    Host
		control Determinism
		message string
	}{
		{"json", json.RawMessage(`{`), diagnosticTestHost{}, control, "capability invocation is invalid"},
		{"size", json.RawMessage(`"` + strings.Repeat("x", MaxInputBytes) + `"`), diagnosticTestHost{}, control, "capability invocation is invalid"},
		{"host", json.RawMessage(`{}`), nil, control, "capability invocation is invalid"},
		{"controls", json.RawMessage(`{}`), diagnosticTestHost{}, Determinism{}, "capability deterministic controls are invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runInProcessWithProgram(context.Background(), nil, program, tc.input, tc.host, tc.control)
			// Reject at the request boundary, before authored schema parsing or execution can run.
			if err == nil || err.Error() != tc.message {
				t.Fatalf("expected %q, got %v", tc.message, err)
			}
		})
	}
}

// assertCachedProgramPhases keeps load-time compilation out of per-invocation measurements.
func assertCachedProgramPhases(t *testing.T, phases []PhaseTiming) {
	t.Helper()
	// The public phase ordering stays stable even though compilation no longer occurs per request.
	if len(phases) != 5 || !ValidExecutionPhases(phases) || phases[0].Duration != 0 {
		t.Fatalf("unexpected cached phases: %#v", phases)
	}
}
