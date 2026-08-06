package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/models"
)

// mcpCallRequest is the body shape callClient.ts's remoteCall sends: the
// operationId to resolve plus a flat params map, exactly as documented in
// that file's header comment (runtime/mcp/src/callClient.ts).
type mcpCallRequest struct {
	OperationID string         `json:"operation_id"`
	Params      map[string]any `json:"params"`
}

// mcpCallResponse mirrors callClient.ts's CallResponse -- exactly one of
// Result/Error is set. Result is left as json.RawMessage rather than a Go
// string so a JSON vendor response reaches the script as a parsed value,
// not a double-encoded string.
type mcpCallResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// mcpCallHandler is call()'s only server-side entrypoint (design doc,
// "No I/O outside call()" -- the shared runtime's sandboxed process has no
// other path to a vendor API). It is intentionally thin: resolve the
// session (only a live, already-authenticated MCP session may call this at
// all), resolve+validate the operation against the fixture (Guarding
// Against Hallucinated Calls), then hand off to the exact same
// engineExecuteCore path the gRPC edge uses (sandbox.go:
// EngineStreamExecuteFunc). No new dispatch or param-routing logic is
// introduced here -- that already exists in dispatcher.go and works the
// same way regardless of which caller flattened params into the map.
func mcpCallHandler(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := extractBearerToken(r)
	if !ok {
		writeMCPCallResult(w, http.StatusUnauthorized, mcpCallResponse{Error: "Authorization header required"})
		return
	}

	sess, ok := lookupMCPSession(sessionID)
	if !ok {
		writeMCPCallResult(w, http.StatusNotFound, mcpCallResponse{Error: "mcp session not found or expired"})
		return
	}

	var req mcpCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMCPCallResult(w, http.StatusBadRequest, mcpCallResponse{Error: "invalid request body"})
		return
	}

	op, ok := resolveSessionFixtureOperation(sess, req.OperationID)
	if !ok {
		// Tier-1 enforcement (design doc, Trust and Governance Model): an
		// operationId outside this server's registered set fails to resolve
		// here, before any credential or vendor is ever touched.
		slog.WarnContext(r.Context(), "mcp call() rejected: unregistered operationId",
			slog.String("operation_id", req.OperationID))
		writeMCPCallResult(w, http.StatusNotFound, mcpCallResponse{Error: fmt.Sprintf("unknown operationId %q", req.OperationID)})
		return
	}

	if err := validateCallParams(op, req.Params); err != nil {
		slog.WarnContext(r.Context(), "mcp call() rejected: schema validation failed",
			slog.String("operation_id", req.OperationID))
		writeMCPCallResult(w, http.StatusBadRequest, mcpCallResponse{Error: err.Error()})
		return
	}

	result, err := dispatchMCPCall(r.Context(), sess, req.OperationID, req.Params)
	if err != nil {
		writeMCPCallResult(w, http.StatusBadGateway, mcpCallResponse{Error: err.Error()})
		return
	}

	writeMCPCallResult(w, http.StatusOK, mcpCallResponse{Result: result})
}

// resolveSessionFixtureOperation requires the app-derived session
// fixture so one tenant can never observe a process-wide operation catalog.
func resolveSessionFixtureOperation(sess *mcpSession, operationID string) (*FixtureOperation, bool) {
	if sess == nil || sess.fixture == nil {
		return nil, false
	}
	return sess.fixture.Resolve(operationID)
}

// lookupMCPSession is a read-only accessor mirroring the RLock pattern used
// throughout mcp.go (e.g. mcpMessageHandler). Kept unexported: /mcp/call is
// the only caller outside mcp.go's own handlers.
func lookupMCPSession(sessionID string) (*mcpSession, bool) {
	mcpSessions.RLock()
	defer mcpSessions.RUnlock()
	sess, ok := mcpSessions.m[sessionID]
	return sess, ok
}

// extractBearerToken reads the session ID off the Authorization header.
// Shared-runtime sessions use their MCP sessionId as this bearer value (see
// callClient.ts) -- it is not a credential, only a lookup key into
// mcpSessions, the same non-secret identifier already used in the SSE
// transport's message URL (/mcp/message?sessionId=...).
func extractBearerToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return "", false
	}
	return token, true
}

// dispatchMCPCall funnels one call() invocation through engineExecuteCore --
// the same credential-resolving, scope-enforcing, audited path the gRPC edge
// uses. Only non-secret user/resource selectors enter the credential envelope;
// globalSecretResolver resolves provider material server-side from the
// app scope and validated identity. The sandboxed Node process therefore
// never holds a provider credential. sess.token (not just sess.appID) remains
// required because engineExecuteCore resolves the account identity from it.
func dispatchMCPCall(ctx context.Context, sess *mcpSession, operationID string, params map[string]any) (json.RawMessage, error) {
	ctx = contextWithExecutionTransport(ctx, models.EngineExecutionTransportMCP)
	ctx = contextWithMCPIdempotencyIdentity(ctx, params)

	buf := engine.NewBufferStream()
	err := engineExecuteCore(
		ctx, globalObjectCache, globalDispatcher, globalTokenValidator,
		sess.appID, sess.token, operationID, params, copyCredentialEnvelope(sess.authContext), "", buf,
	)
	if err != nil {
		return nil, err
	}
	return bufferToJSONResult(buf.Bytes())
}

// copyCredentialEnvelope prevents concurrent MCP calls from sharing a map
// that the secret resolver enriches with decrypted, request-local material.
func copyCredentialEnvelope(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// bufferToJSONResult forwards the vendor's response as a parsed JSON value
// when possible, so an execute script can do `const repos = await
// call(...); return repos.length` instead of having to JSON.parse a string
// itself. A non-JSON vendor response (e.g. plain text, or an empty body) is
// still returned rather than discarded or turned into an error -- as a JSON
// string -- so a shape mismatch is something the script/model can see and
// react to, not a silently eaten response.
func bufferToJSONResult(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	if json.Valid(raw) {
		return json.RawMessage(raw), nil
	}
	encoded, err := json.Marshal(string(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to encode vendor response: %w", err)
	}
	return json.RawMessage(encoded), nil
}

// writeMCPCallResult is this handler's own JSON writer rather than the
// package's existing writeError (mcp.go): writeError builds its body with
// fmt.Sprintf and does not escape the message, which breaks as soon as an
// error string contains a quote -- something this handler's validation
// errors do routinely (e.g. `missing required parameter "foo"`). Encoding
// through encoding/json avoids shipping a client-observable malformed-JSON
// bug instead of fixing it here.
func writeMCPCallResult(w http.ResponseWriter, status int, resp mcpCallResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
