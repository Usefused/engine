package executionappvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

type diagnosticTestHost struct{}

// Fetch provides bounded JSON without making a provider request during exception tests.
func (diagnosticTestHost) Fetch(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// DBGet keeps the test independent of persistence.
func (diagnosticTestHost) DBGet(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`null`), nil
}

// DBSet satisfies the interpreter host contract without a side effect.
func (diagnosticTestHost) DBSet(context.Context, json.RawMessage) error { return nil }

// TestPrivateExceptionDiagnostics verifies phases, thrown primitives, and safe public error strings.
func TestPrivateExceptionDiagnostics(t *testing.T) {
	tests := []struct{ name, input, execute, output, phase, text string }{
		{"input", `throw new Error("private input path: customer.id")`, `return value`, `return value`, "input_validation", "private input path"},
		{"execute", `return value`, `throw new Error("private customer token")`, `return value`, "execute", "private customer token"},
		{"primitive", `return value`, `throw "private string"`, `return value`, "execute", "private string"},
		{"output", `return value`, `return {privateOutput:"retained"}`, `throw new Error("private output path")`, "output_validation", "private output path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bundle := `globalThis.FusedUnifiedApp={input:{parse(value){` + tc.input + `}},output:{parse(value){` + tc.output + `}},execute:async(value)=>{` + tc.execute + `}};`
			_, err := RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), diagnosticTestHost{})
			detail := PrivateDiagnostic(err)
			// Raw exceptions belong only to the explicit private projection, including non-Error throws.
			if detail == nil || detail.Phase != tc.phase || !strings.Contains(detail.Message, tc.text) || strings.Contains(err.Error(), "private") {
				t.Fatalf("unexpected private/public error: %#v, %v", detail, err)
			}
			// A rejected output schema must retain the value that failed validation.
			if tc.name == "output" && !strings.Contains(detail.Response, "privateOutput") {
				t.Fatalf("missing rejected output: %#v", detail)
			}
		})
	}
}

// TestDiagnosticThrowingGetterCannotHidePhase ensures hostile exception objects remain safe to inspect.
func TestDiagnosticThrowingGetterCannotHidePhase(t *testing.T) {
	bundle := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{throw {toString(){return "private getter"},get stack(){throw new Error("no")}}}};`
	_, err := RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), diagnosticTestHost{})
	detail := PrivateDiagnostic(err)
	// A stack getter failure must preserve the already-captured phase and message.
	if detail == nil || detail.Phase != "execute" || detail.Message != "private getter" {
		t.Fatalf("unexpected diagnostic: %#v", detail)
	}
}

// TestDiagnosticStackUsesTypeScriptSourceMap proves minified frames resolve to authored source coordinates.
func TestDiagnosticStackUsesTypeScriptSourceMap(t *testing.T) {
	sourceMap := base64.StdEncoding.EncodeToString([]byte(`{"version":3,"sources":["unified-app.ts"],"names":[],"mappings":"AAiBA;AAAA"}`))
	bundle := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{throw new Error("private mapped error")}};` + "\n//# sourceMappingURL=data:application/json;base64," + sourceMap
	_, err := RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), diagnosticTestHost{})
	detail := PrivateDiagnostic(err)
	// The retained frame must name the TypeScript source line instead of the bundled JavaScript column.
	if detail == nil || !strings.Contains(detail.Stack, "unified-app.ts:18") {
		t.Fatalf("unmapped stack: %#v", detail)
	}
}
