package executionappvm

import (
	"context"
	"encoding/json"
	"testing"
)

type phaseTestHost struct {
	diagnosticTestHost
	phases []PhaseTiming
}

// RecordExecutionPhases observes the actual interpreter exit without a provider or database dependency.
func (host *phaseTestHost) RecordExecutionPhases(_ context.Context, phases []PhaseTiming) {
	host.phases = phases
}

// TestExecutionPhasesIncludeAuthoredFailures verifies real stage evidence survives both success and an exception.
func TestExecutionPhasesIncludeAuthoredFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		host := &phaseTestHost{}
		execute := `return {ok:true}`
		// The failure happens in authored code, before any provider operation exists.
		if fail {
			execute = `throw new Error("private failure")`
		}
		bundle := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{` + execute + `}};`
		_, err := RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), host)
		expected := 5
		// A failed execute must not manufacture an output-validation measurement.
		if fail {
			expected = 4
		}
		if (err != nil) != fail || len(host.phases) != expected || !ValidExecutionPhases(host.phases) || host.phases[expected-1].Failed != fail {
			t.Fatalf("bad phases: %#v %v", host.phases, err)
		}
	}
}
