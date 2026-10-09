package sandbox

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

// importedNativeWriter captures a bounded server-owned JSON-RPC reply for delivery over a legacy SSE session.
type importedNativeWriter struct {
	header http.Header
	body   bytes.Buffer
}

// Header supplies the common native handler's HTTP metadata without changing the SSE stream's headers.
func (w *importedNativeWriter) Header() http.Header { return w.header }

// WriteHeader is intentionally ignored because JSON-RPC errors travel in the SSE payload.
func (w *importedNativeWriter) WriteHeader(int) {}

// Write captures protocol envelopes which are already bounded by the imported catalog and invocation budgets.
func (w *importedNativeWriter) Write(raw []byte) (int, error) { return w.body.Write(raw) }

// handleImportedSSENative routes only initialized native requests to the shared Go capability handler.
func handleImportedSSENative(ctx context.Context, w http.ResponseWriter, sess *mcpSession, body []byte) bool {
	request, err := parseMCPJSONRPCRequest(body)
	// Tools and initialization retain their existing child-runtime transport path.
	if err != nil || (!strings.HasPrefix(request.Method, "prompts/") && !strings.HasPrefix(request.Method, "resources/")) {
		return false
	}
	sess.lifecycleMu.Lock()
	active := mcpSessionRegisteredAndActiveLocked(sess) && sess.initialized
	sess.lifecycleMu.Unlock()
	// Native operations cannot execute before a successful handshake or after retirement.
	if !active {
		writeMCPJSONRPCError(w, request.ID, -32600, "initialized MCP session required", http.StatusBadRequest)
		return true
	}
	callCtx, cancel := mcpSessionRequestContext(ctx, sess)
	defer cancel()
	captured := &importedNativeWriter{header: http.Header{}}
	if !handleImportedSessionNative(callCtx, captured, sess, request) {
		return false
	}
	// The bounded session queue supplies backpressure and cancels with the admitted request.
	select {
	case sess.serverNotifications <- captured.body.String():
		touchMCPSession(sess)
		w.WriteHeader(http.StatusAccepted)
	case <-callCtx.Done():
		w.WriteHeader(http.StatusRequestTimeout)
	}
	return true
}

// importedSSEInitializeResponse advertises the same native namespaces as Streamable HTTP initialization.
func importedSSEInitializeResponse(sessionID, response string) string {
	sess, ok := lookupMCPSession(sessionID)
	// Retired sessions cannot acquire a fresh provider-capability advertisement.
	if !ok {
		return response
	}
	return importedInitializeResponse(response, sess.fixture)
}

// isImportedInitializeResult prevents capability metadata from being injected into unrelated tool results.
func isImportedInitializeResult(result map[string]any) bool {
	version, ok := result["protocolVersion"].(string)
	return ok && version != ""
}

// importedSSENotifications returns the already registered session queue without creating a second transport lifecycle.
func importedSSENotifications(sessionID string) <-chan string {
	sess, ok := lookupMCPSession(sessionID)
	// A missing session leaves a disabled select case while child shutdown drains normally.
	if !ok {
		return nil
	}
	return sess.serverNotifications
}
