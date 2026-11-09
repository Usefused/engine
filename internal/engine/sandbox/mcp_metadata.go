package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/Usefused/engine/internal/engine/executionappvm"
	engineruntime "github.com/Usefused/engine/runtime"
)

// Compile only bundled trusted code once; request data is always passed as JSON, never as source.
var mcpMetadataProgram = sync.OnceValues(func() (*executionappvm.TrustedProgram, error) {
	return executionappvm.CompileTrustedProgram("fused-metadata.js", engineruntime.MCPMetadataBundle)
})

// runMCPMetadata evaluates trusted catalogue logic through the shared bounded Goja runtime.
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
	var input []byte
	// Only documentation search needs an authorized catalogue; tool declarations contain no operation data.
	if fixture != nil {
		input, err = json.Marshal(map[string]any{"catalogue": fixture, "arguments": arguments})
		// Non-JSON values must never be coerced into executable source or silently omitted.
		if err != nil {
			return nil, errors.New("MCP metadata input is invalid")
		}
	}
	return executionappvm.EvaluateMCPMetadata(ctx, program, input, maxMCPPhysicalResultBytes)
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
