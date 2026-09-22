package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	engineruntime "github.com/Usefused/engine/runtime"
	"github.com/dop251/goja"
)

var mcpMetadataWords = regexp.MustCompile(`[\p{L}\p{N}]+`)

// Compile only bundled trusted code once; request data is always passed as JSON, never as source.
var mcpMetadataProgram = sync.OnceValues(func() (*goja.Program, error) {
	return goja.Compile("fused-metadata.js", engineruntime.MCPMetadataBundle, true)
})

// runMCPMetadata evaluates trusted catalogue logic with request-local memory and no I/O or execution helpers.
func runMCPMetadata(ctx context.Context, fixture *Fixture, arguments map[string]any) (map[string]any, error) {
	// Cancellation is authoritative even before the asynchronous interrupt callback is scheduled.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	program, err := mcpMetadataProgram()
	// A broken embedded build must fail closed instead of silently starting a child process.
	if err != nil {
		return nil, errors.New("MCP metadata bundle is unavailable")
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(512)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { vm.Interrupt("metadata request cancelled") })
	defer stop()
	// The existing serializer needs only UTF-8 byte counts; do not expose Node or native objects.
	if err := vm.Set("Buffer", map[string]any{"byteLength": func(value string) int { return len(value) }}); err != nil {
		return nil, errors.New("MCP metadata serializer is unavailable")
	}
	// Go and Node both implement Unicode category L/N; Goja does not yet implement RegExp property escapes.
	if err := vm.Set("fusedMetadataUnicodeWords", func(value string) []string {
		return append([]string{}, mcpMetadataWords.FindAllString(value, -1)...)
	}); err != nil {
		return nil, errors.New("MCP metadata Unicode support is unavailable")
	}
	// Loading trusted code in a fresh interpreter prevents catalogue data crossing requests or tokens.
	if _, err := vm.RunProgram(program); err != nil {
		return nil, errors.New("MCP metadata initialization failed")
	}
	return invokeMCPMetadata(vm, fixture, arguments)
}

// invokeMCPMetadata separates untrusted JSON from the finite trusted entrypoints.
func invokeMCPMetadata(vm *goja.Runtime, fixture *Fixture, arguments map[string]any) (map[string]any, error) {
	source := "JSON.stringify(FusedMetadata.listTools())"
	// Only documentation search needs an authorized catalogue; tool declarations contain no operation data.
	if fixture != nil {
		payload, err := json.Marshal(map[string]any{"catalogue": fixture, "arguments": arguments})
		// Non-JSON values must never be coerced into executable source or silently omitted.
		if err != nil {
			return nil, errors.New("MCP metadata input is invalid")
		}
		// JSON parsing creates plain interpreter-owned values, without exposing Go object methods.
		if err := vm.Set("metadataJSON", string(payload)); err != nil {
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
	if len(value.String()) > maxMCPPhysicalResultBytes || json.Unmarshal([]byte(value.String()), &result) != nil || result == nil {
		return nil, errors.New("MCP metadata result is invalid")
	}
	return result, nil
}

// handleMCPModernSearchDocs reads the exact token-authorized catalogue without allocating sandbox state.
func handleMCPModernSearchDocs(ctx context.Context, w http.ResponseWriter, request mcpJSONRPCRequest, admission *mcpModernAdmission, arguments map[string]any) {
	appID := admission.target.AppID.String()
	// Cache ownership is scoped to this read so concurrent execution retains its independent reference.
	if err := globalObjectCache.ConnectSDK(ctx, appID); err != nil {
		writeMCPModernError(w, request.ID, -32603, "MCP documentation is unavailable", http.StatusInternalServerError, nil)
		return
	}
	defer globalObjectCache.DisconnectSDK(appID)
	fixture, err := prepareSessionFixture(ctx, appID, admission.identity.TokenPolicy)
	// This is the same token, selection, Unified descriptor, and schema admission boundary used by execution.
	if err != nil {
		writeMCPModernError(w, request.ID, -32603, "MCP documentation is unavailable", http.StatusInternalServerError, nil)
		return
	}
	observation := startMCPSearchObservation(ctx, request, &mcpSession{fixture: fixture, transport: mcpModernToolTransport})
	result, err := runMCPDocumentation(ctx, fixture, arguments)
	// Retain privacy-safe search telemetry even though no process/session exists for metadata requests.
	if err != nil {
		finishMCPSearchObservation(observation, "", "runtime_unavailable")
		writeMCPModernError(w, request.ID, -32603, "MCP documentation search failed", http.StatusInternalServerError, nil)
		return
	}
	envelope, _ := json.Marshal(map[string]any{"result": result})
	finishMCPSearchObservation(observation, string(envelope), "")
	writeMCPModernResult(w, request.ID, result, admission.server)
}
