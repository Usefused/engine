package api

import (
	"github.com/Usefused/engine/internal/engine/sandbox"
	"testing"
)

// TestCapabilityAdmissionPublicOutcome preserves actionable queue failures without disclosing authored diagnostics.
func TestCapabilityAdmissionPublicOutcome(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{sandbox.ErrCapabilityAdmissionFull, "execution_capacity_exceeded"},
		{sandbox.ErrCapabilityAdmissionTimeout, "execution_queue_timeout"},
		{sandbox.ErrCapabilityWorkerOverloaded, "execution_capacity_exceeded"},
	}
	for _, test := range cases {
		state, code := capabilityCompletion(test.err, nil, false)
		// Admission never dispatched authored work, so its terminal failure category is unambiguous.
		if state != "failed" || code != test.code {
			t.Fatalf("outcome=%s/%s", state, code)
		}
		message := capabilityPublicError(code)
		// Capacity guidance must not collapse back into a generic authored failure.
		if message == "" || message == "unified app did not complete successfully" {
			t.Fatalf("missing capacity message: %q", message)
		}
	}
}
