package sandbox

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"strings"
	"testing"
)

// TestCapabilityPrivateDiagnosticFrame verifies worker IPC preserves detail without exposing it through ordinary errors.
func TestCapabilityPrivateDiagnosticFrame(t *testing.T) {
	raw, _ := json.Marshal(capabilityProcessFrame{Kind: "done", Error: "capability execution failed", Diagnostic: &executionappvm.DiagnosticError{Phase: "execute", Message: "private exception", Stack: "unified-app.ts:18:3"}})
	frame, err := decodeCapabilityFrame(raw)
	// Valid trusted-worker frames retain their separate private diagnostic field.
	if err != nil {
		t.Fatal(err)
	}
	_, err = capabilityProcessOutput(frame)
	detail := executionappvm.PrivateDiagnostic(err)
	// Error() can pass through logs and caller adapters without revealing private frames or messages.
	if detail == nil || detail.Message != "private exception" || detail.Stack != "unified-app.ts:18:3" || strings.Contains(err.Error(), "private") {
		t.Fatalf("lost or disclosed worker diagnostic: %#v %v", detail, err)
	}
}
