package executionappvm

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/dop251/goja"
)

// metadataWords matches Unicode letters and digits; Goja does not yet implement RegExp property escapes.
var metadataWords = regexp.MustCompile(`[\p{L}\p{N}]+`)

// metadataTimeout bounds one trusted catalogue evaluation.
const metadataTimeout = 5 * time.Second

// TrustedProgram is compiled trusted bytecode shipped inside the Engine binary, never authored or request source.
type TrustedProgram struct{ program *goja.Program }

// CompileTrustedProgram compiles bundled trusted JavaScript once so callers can reuse its immutable bytecode.
func CompileTrustedProgram(name, source string) (*TrustedProgram, error) {
	program, err := goja.Compile(name, source, true)
	if err != nil {
		return nil, err
	}
	return &TrustedProgram{program: program}, nil
}

// EvaluateMCPMetadata runs the trusted MCP catalogue logic in a fresh bounded runtime with no I/O or execution helpers.
// A nil input lists tool declarations; otherwise input is the JSON {"catalogue": ..., "arguments": ...} for search.
func EvaluateMCPMetadata(ctx context.Context, trusted *TrustedProgram, input []byte, maxResultBytes int) (map[string]any, error) {
	// Cancellation is authoritative even before the asynchronous interrupt callback is scheduled.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A broken embedded build must fail closed instead of silently starting a child process.
	if trusted == nil || trusted.program == nil {
		return nil, errors.New("MCP metadata bundle is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	vm, stop := newBoundedRuntime(ctx, "metadata request cancelled")
	defer stop()
	// The existing serializer needs only UTF-8 byte counts; do not expose Node or native objects.
	if err := vm.Set("Buffer", map[string]any{"byteLength": func(value string) int { return len(value) }}); err != nil {
		return nil, errors.New("MCP metadata serializer is unavailable")
	}
	// Go and Node both implement Unicode category L/N.
	if err := vm.Set("fusedMetadataUnicodeWords", func(value string) []string {
		return append([]string{}, metadataWords.FindAllString(value, -1)...)
	}); err != nil {
		return nil, errors.New("MCP metadata Unicode support is unavailable")
	}
	// Loading trusted code in a fresh interpreter prevents catalogue data crossing requests or tokens.
	if _, err := vm.RunProgram(trusted.program); err != nil {
		return nil, errors.New("MCP metadata initialization failed")
	}
	source := "JSON.stringify(FusedMetadata.listTools())"
	// Only documentation search needs an authorized catalogue; tool declarations contain no operation data.
	if input != nil {
		// JSON parsing creates plain interpreter-owned values, without exposing Go object methods.
		if err := vm.Set("metadataJSON", string(input)); err != nil {
			return nil, errors.New("MCP metadata input is unavailable")
		}
		source = "var input = JSON.parse(metadataJSON); JSON.stringify(FusedMetadata.search(input.catalogue, input.arguments))"
	}
	value, err := vm.RunString(source)
	// Interpreter errors may include authored schema fragments and therefore remain private.
	if err != nil {
		return nil, errors.New("MCP metadata evaluation failed")
	}
	var result map[string]any
	// A malformed or oversized trusted result is a build/runtime failure, never an execution fallback.
	if len(value.String()) > maxResultBytes || json.Unmarshal([]byte(value.String()), &result) != nil || result == nil {
		return nil, errors.New("MCP metadata result is invalid")
	}
	return result, nil
}
