package executionappvm

import (
	"context"
	"errors"
	"strings"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// DiagnosticError crosses trusted worker IPC separately from its safe public Error string.
type DiagnosticError struct {
	Phase     string `json:"phase"`
	Message   string `json:"message"`
	Stack     string `json:"stack,omitempty"`
	Response  string `json:"response,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Error prevents implicit logging from exposing authored values; API responses explicitly project Message.
func (*DiagnosticError) Error() string { return "capability execution failed" }

// BoundDiagnostic limits retained error text without censoring its debugging content.
func BoundDiagnostic(value string) string {
	// Storage and IPC stay finite even when authored code throws a huge value.
	if len(value) > 65536 {
		return strings.ToValidUTF8(value[:65536], "")
	}
	return value
}

// PrivateDiagnostic preserves full details for private diagnostics; ordinary result projections select only Message.
func PrivateDiagnostic(err error) *DiagnosticError {
	var detail *DiagnosticError
	// Plain infrastructure failures also need an actionable internal message.
	if errors.As(err, &detail) {
		return detail
	}
	// Successful executions retain call evidence without inventing an exception.
	if err == nil {
		return nil
	}
	return runtimeDiagnostic(err, "runtime")
}

// exceptionDiagnostic extracts bounded error strings while the VM remains inside its execution deadline.
func exceptionDiagnostic(vm *goja.Runtime, value goja.Value) (detail *DiagnosticError) {
	detail = &DiagnosticError{Phase: "execute", Message: "Unhandled exception"}
	// Throwing getters must not escape diagnostics capture or prevent terminal persistence.
	defer func() { _ = recover() }()
	phase := vm.Get("__fusedExecutionPhase")
	// The phase is diagnostic data, never execution or authorization authority.
	if phase != nil && !goja.IsUndefined(phase) {
		detail.Phase = BoundDiagnostic(phase.String())
	}
	output := vm.Get("__fusedRawOutput")
	// Preserve an invalid output before inspecting potentially hostile error getters.
	if output != nil && !goja.IsUndefined(output) {
		detail.Response = BoundDiagnostic(output.String())
		detail.Truncated = len(output.String()) > 65536
	}
	message := value.String()
	detail.Message = BoundDiagnostic(message)
	detail.Truncated = detail.Truncated || len(message) > 65536
	// JavaScript permits throwing null and primitives, which do not necessarily have a stack.
	if goja.IsNull(value) || goja.IsUndefined(value) {
		return detail
	}
	stack := value.ToObject(vm).Get("stack")
	if stack != nil && !goja.IsUndefined(stack) && !goja.IsNull(stack) {
		text := stack.String()
		detail.Stack = BoundDiagnostic(text)
		detail.Truncated = detail.Truncated || len(text) > 65536
	}
	return detail
}

// runtimeDiagnostic retains interpreter failures behind the same safe public error boundary.
func runtimeDiagnostic(err error, phase string) *DiagnosticError {
	message := err.Error()
	var interrupted *goja.InterruptedError
	// Interpreter interruption and deadline failures must not look like ordinary authored exceptions.
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &interrupted) {
		phase = "timeout"
	}
	return &DiagnosticError{Phase: phase, Message: BoundDiagnostic(message), Truncated: len(message) > 65536}
}

// rejectExternalSourceMap keeps diagnostics from granting filesystem or network access to authored code.
func rejectExternalSourceMap(string) ([]byte, error) {
	return nil, errors.New("only inline source maps are supported")
}

// compileDiagnosticBundle permits embedded source maps while denying out-of-bundle dependencies.
func compileDiagnosticBundle(name, source string) (*goja.Program, error) {
	parsed, err := goja.Parse(name, source, parser.WithSourceMapLoader(rejectExternalSourceMap))
	// Invalid source cannot fall through to a less restrictive compilation path.
	if err != nil {
		return nil, err
	}
	return goja.CompileAST(parsed, true)
}

// compileCapabilityBundle bounds authored source before parsing inside the confined child.
func compileCapabilityBundle(bundle []byte) (*goja.Program, error) {
	// Load and one-off execution share the same source limits and source-map policy.
	if len(bundle) == 0 || len(bundle) > MaxBundleBytes {
		return nil, errors.New("capability bundle is invalid")
	}
	return compileDiagnosticBundle("fused-capability.js", string(bundle))
}
