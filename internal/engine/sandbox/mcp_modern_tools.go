package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/mcpsession"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

const (
	mcpModernToolTransport          = "modern_state_handle"
	mcpModernChildProtocolVersion   = "2025-11-25"
	mcpModernStateHandleArgument    = "stateHandle"
	mcpModernStateMetadataKey       = "com.usefused/state"
	mcpModernStateHandlePrefix      = "fused-state:"
	mcpModernInternalInitializeID   = `"fused-modern-initialize"`
	mcpModernExecuteToolDescription = "Run TypeScript that invokes only operations discovered with search_docs through await call(). Pass stateHandle only when continuing session.get, session.set, session.page, or a retained-result next_request from an earlier execute result; omit it for independent work. Follow structured recovery exactly and never replay provider mutations after an unknown outcome."
)

var mcpModernToolHandles = struct {
	sync.RWMutex
	byHandle map[string]string
}{byHandle: make(map[string]string)}

// mcpModernRuntimeFailure carries one bounded transport failure without exposing child or handle identity.
type mcpModernRuntimeFailure struct {
	status  int
	code    int
	message string
	data    any
}

// mcpModernChildEnvelope decodes only the result-or-error boundary returned by the internal legacy SDK process.
type mcpModernChildEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  map[string]any  `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	} `json:"error"`
}

// handleMCPModernToolsList serves the shared tool definitions without allocating a child or consuming a sandbox-start token.
func handleMCPModernToolsList(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	// Connected-user selector validation remains identical to execution even though declarations are public within this app.
	if _, err := mcpSessionAuthContext(r.Header); err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	result, err := runMCPMetadata(ctx, nil, nil)
	// A bad embedded catalogue is a server failure and must never trigger legacy child startup.
	if err != nil {
		writeMCPModernError(w, request.ID, -32603, "MCP tool catalogue is unavailable", http.StatusInternalServerError, nil)
		return
	}
	// Modern execute retains its explicit continuation contract over the shared compatibility descriptor.
	if err := adaptMCPModernToolList(result); err != nil {
		writeMCPModernError(w, request.ID, -32603, "MCP tool catalogue is invalid", http.StatusInternalServerError, nil)
		return
	}
	writeMCPModernResult(w, request.ID, result, admission.server)
}

// handleMCPModernToolsCall executes one tool and retains sandbox state only behind an explicit token-bound handle.
func handleMCPModernToolsCall(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	params, arguments, handle, err := admitMCPModernToolCall(request)
	// Invalid explicit state must fail before allocating a child or invoking provider-capable code.
	if err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	authContext, err := mcpSessionAuthContext(r.Header)
	// Per-request selectors remain outside model-authored tool arguments and bind every reused handle.
	if err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	// Metadata reads need the authorized catalogue but neither an execution child nor continuation state.
	if params["name"] == mcpSearchToolName {
		handleMCPModernSearchDocs(ctx, w, request, admission, arguments)
		return
	}
	// Unknown names cannot consume process capacity merely to discover that no such tool exists.
	if params["name"] != "execute" {
		writeMCPModernError(w, request.ID, -32602, "unknown MCP tool", http.StatusBadRequest, nil)
		return
	}
	var sess *mcpSession
	keepState := params["name"] == "execute"
	mintedState := keepState && handle == ""
	// An explicit handle may continue only the exact app, token, route, and selector context that minted it.
	if handle != "" {
		sess = resolveMCPModernToolHandle(handle, routeID, admission, authContext)
		if sess == nil {
			writeMCPModernError(w, request.ID, -32602, "stateHandle is unavailable or does not belong to this request context", http.StatusNotFound, mcpModernStateUnavailableData())
			return
		}
	} else {
		var failure *mcpModernRuntimeFailure
		sess, failure = startMCPModernToolRuntime(ctx, span, w, r, routeID, token, request, admission, keepState)
		// Independent executions receive isolated children; only explicit state handles reuse them.
		if failure != nil {
			writeMCPModernRuntimeFailure(w, request.ID, failure)
			return
		}
	}
	// Stateless tools and failed state registration release their child at this request boundary.
	if !keepState {
		defer terminateMCPSession(sess.sessionID, "client_terminated")
	}
	if keepState && handle == "" {
		var registered bool
		handle, registered = registerMCPModernToolHandle(sess)
		// A concurrently retired runtime must not mint a handle that can never resolve.
		if !registered {
			terminateMCPSession(sess.sessionID, "runtime_failed")
			writeMCPModernError(w, request.ID, -32603, "MCP tool state could not be created", http.StatusServiceUnavailable, nil)
			return
		}
	}
	childRequest, err := mcpModernChildRequest(request, arguments)
	// Adapter encoding failure is pre-provider; only a caller-visible existing handle can remain reusable.
	if err != nil {
		// A newly minted handle has not reached the caller and must not leave unreachable state behind.
		if mintedState {
			terminateMCPSession(sess.sessionID, "client_terminated")
		} else if keepState && handle != "" {
			touchMCPSession(sess)
		}
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	response, failure := exchangeMCPModernChild(ctx, sess, childRequest)
	// Runtime failure retires the handle through the shared child-session cleanup path.
	if failure != nil {
		terminateMCPSession(sess.sessionID, "runtime_failed")
		writeMCPModernRuntimeFailure(w, request.ID, failure)
		return
	}
	stateReturned := !mintedState
	transform := func(result map[string]any) error {
		// Only execute reaches this path and owns cross-request sandbox state.
		if keepState {
			isError, _ := result["isError"].(bool)
			// A failed first execution created no useful caller state, while an existing handle remains valid across tool errors.
			if mintedState && isError {
				return nil
			}
			attachMCPModernStateHandle(result, handle)
			stateReturned = true
		}
		return nil
	}
	deliveredState := writeMCPModernChildResponse(w, request.ID, response, admission.server, transform)
	// A rejected first execute call cannot retain its new handle, so its otherwise unreachable child must be retired immediately.
	if mintedState && (!deliveredState || !stateReturned) {
		terminateMCPSession(sess.sessionID, "client_terminated")
	}
}

// startMCPModernToolRuntime creates and internally initializes one isolated child without exposing a protocol session.
func startMCPModernToolRuntime(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string, request mcpJSONRPCRequest, admission *mcpModernAdmission, retainState bool) (*mcpSession, *mcpModernRuntimeFailure) {
	// Each newly allocated child consumes the same connection-start budget as the compatibility transport.
	if failure := allowMCPModernToolRuntimeStart(ctx, span, w, admission.target.AppID.String()); failure != nil {
		return nil, failure
	}
	authContext, err := mcpSessionAuthContext(r.Header)
	// Invalid connected-user selectors cannot enter child-owned execution state.
	if err != nil {
		return nil, &mcpModernRuntimeFailure{status: http.StatusBadRequest, code: -32602, message: err.Error()}
	}
	metadata := mcpModernClientMetadata(r, request)
	// Execute handles remain visible in lifecycle history; metadata requests never enter this startup path.
	sess, err := startMCPRuntimeSession(ctx, routeID, admission.target.AppID.String(), token, mcpModernProtocolVersion, mcpModernToolTransport, !retainState, false, authContext, admission.identity, metadata)
	// Catalogue preparation and process startup fail before any tool or provider request can run.
	if err != nil {
		status, _ := mcpSessionStartFailure(err)
		return nil, &mcpModernRuntimeFailure{status: status, code: -32603, message: "MCP tool runtime could not start"}
	}
	sess.metadataMu.Lock()
	// The private compatibility handshake must not overwrite metadata admitted from the public 2026 request.
	sess.clientInfoRecorded = true
	sess.metadataMu.Unlock()
	if failure := initializeMCPModernToolRuntime(ctx, sess); failure != nil {
		terminateMCPSession(sess.sessionID, "runtime_failed")
		return nil, failure
	}
	return sess, nil
}

// allowMCPModernToolRuntimeStart applies shared process-rate and concurrency limits without allocating a child.
func allowMCPModernToolRuntimeStart(ctx context.Context, span trace.Span, w http.ResponseWriter, appID string) *mcpModernRuntimeFailure {
	// Stateless wire semantics do not permit unbounded process allocation per request.
	if sessionStartRateLimiter != nil && !sessionStartRateLimiter.allow(appID) {
		w.Header().Set("Retry-After", "60")
		return &mcpModernRuntimeFailure{status: http.StatusTooManyRequests, code: -32000, message: "too many tool runtimes for this MCP server, please slow down"}
	}
	limitErr := entitlement.CheckLimit(span, "mcp_sandbox_concurrency", activeMCPSessionCount(), entitlement.LiveEntitlement.Load().MaxSandboxConcurrency)
	// Entitlement failure must use the same process count as compatibility sessions and explicit state handles.
	if limitErr != nil {
		return &mcpModernRuntimeFailure{status: http.StatusPaymentRequired, code: -32000, message: limitErr.Error()}
	}
	return nil
}

// initializeMCPModernToolRuntime performs the private 2025 handshake required only by the bundled child SDK.
func initializeMCPModernToolRuntime(ctx context.Context, sess *mcpSession) *mcpModernRuntimeFailure {
	request := mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage(mcpModernInternalInitializeID), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"` + mcpModernChildProtocolVersion + `","capabilities":{},"clientInfo":{"name":"Fused modern adapter","version":"1.0.0"}}`),
	}
	response, failure := exchangeMCPModernChild(ctx, sess, request)
	// The internal adapter cannot serve 2026 tools unless its legacy child completed a known handshake.
	if failure != nil {
		return failure
	}
	version, valid := mcpInitializeResultProtocolVersion(response)
	// A counter-offer would make the adapter's private request semantics ambiguous.
	if !valid || version != mcpModernChildProtocolVersion {
		return &mcpModernRuntimeFailure{status: http.StatusBadGateway, code: -32603, message: fmt.Sprintf("MCP tool runtime returned unsupported internal protocol %q", version)}
	}
	// Durable lifecycle metadata describes the public 2026 handle, not the private child compatibility revision.
	if !commitMCPStreamableInitialize(sess, mcpModernProtocolVersion) {
		return &mcpModernRuntimeFailure{status: http.StatusServiceUnavailable, code: -32603, message: "MCP tool runtime became unavailable during initialization"}
	}
	notification := mcpJSONRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized", Params: json.RawMessage(`{}`)}
	_, failure = exchangeMCPModernChild(ctx, sess, notification)
	return failure
}

// exchangeMCPModernChild serializes one private child request and preserves conservative provider-outcome recovery.
func exchangeMCPModernChild(ctx context.Context, sess *mcpSession, request mcpJSONRPCRequest) (string, *mcpModernRuntimeFailure) {
	body, err := json.Marshal(request)
	// Adapter serialization is a local pre-dispatch failure.
	if err != nil {
		return "", &mcpModernRuntimeFailure{status: http.StatusInternalServerError, code: -32603, message: "MCP tool request could not be encoded"}
	}
	sess.requestMu.Lock()
	defer sess.requestMu.Unlock()
	callID, written, writeErr, active := writeMCPChildRequestLocked(ctx, sess, body, request)
	// A retired explicit handle cannot receive new child bytes.
	if !active {
		return "", &mcpModernRuntimeFailure{status: http.StatusNotFound, code: -32602, message: "MCP tool state is unavailable", data: mcpModernStateUnavailableData()}
	}
	// Partial child delivery preserves unknown provider outcome for execute and safe retry for metadata calls.
	if writeErr != nil {
		failure := mcpDispatchFailure(request, written)
		completeMCPToolCall(sess, callID, "", "dispatch_failed")
		return "", &mcpModernRuntimeFailure{status: http.StatusBadGateway, code: mcpJSONRPCTransportErrorCode, message: "MCP runtime request dispatch failed", data: failure}
	}
	// The private initialized notification is complete once its full line reaches the child.
	if len(request.ID) == 0 {
		return "", nil
	}
	response, err := waitForMCPStreamableResponse(ctx, sess, request.ID)
	// A missing execute response remains unsafe to replay because provider dispatch may have occurred.
	if err != nil {
		failure := mcpRuntimeFailureForRequest(request, mcpRuntimeResponseFailedCode, "runtime_response", "dispatched")
		completeMCPToolCall(sess, callID, "", "runtime_unavailable")
		return "", &mcpModernRuntimeFailure{status: http.StatusBadGateway, code: mcpJSONRPCTransportErrorCode, message: "MCP runtime response unavailable", data: failure}
	}
	completeMCPToolCall(sess, callID, response, "")
	return response, nil
}

// admitMCPModernToolCall strips the adapter-owned state handle while preserving the child's public arguments.
func admitMCPModernToolCall(request mcpJSONRPCRequest) (map[string]any, map[string]any, string, error) {
	var params map[string]any
	// A tool call must be an object before routed name validation or state admission can proceed.
	if json.Unmarshal(request.Params, &params) != nil || params == nil {
		return nil, nil, "", errors.New("tools/call params are invalid")
	}
	name, _ := params["name"].(string)
	arguments, ok := params["arguments"].(map[string]any)
	// Omitted arguments are the standard empty object, while scalar and array arguments remain invalid.
	if params["arguments"] == nil {
		arguments = map[string]any{}
	} else if !ok {
		return nil, nil, "", errors.New("tools/call arguments must be an object")
	}
	handle := ""
	rawHandle, supplied := arguments[mcpModernStateHandleArgument]
	// Only execute owns state; accepting a handle on another tool would imply nonexistent shared semantics.
	if supplied && name != "execute" {
		return nil, nil, "", errors.New("stateHandle is supported only by execute")
	}
	if supplied {
		var valid bool
		handle, valid = rawHandle.(string)
		// Exact prefix and length validation prevents arbitrary model text from becoming a state lookup key.
		if !valid || !strings.HasPrefix(handle, mcpModernStateHandlePrefix) || len(handle) > 128 {
			return nil, nil, "", errors.New("stateHandle is invalid")
		}
		delete(arguments, mcpModernStateHandleArgument)
	}
	delete(params, "_meta")
	params["arguments"] = arguments
	return params, arguments, handle, nil
}

// mcpModernChildRequest converts public per-request metadata into the private child's sessionful parameter shape.
func mcpModernChildRequest(request mcpJSONRPCRequest, admittedArguments map[string]any) (mcpJSONRPCRequest, error) {
	var params map[string]any
	// All modern tool methods carry an object because their required metadata was already admitted.
	if json.Unmarshal(request.Params, &params) != nil || params == nil {
		return mcpJSONRPCRequest{}, errors.New(request.Method + " params are invalid")
	}
	delete(params, "_meta")
	// A tools/call admission supplies the exact argument object after removing Engine-owned state routing.
	if admittedArguments != nil {
		params["arguments"] = admittedArguments
	}
	encoded, err := json.Marshal(params)
	// JSON-compatible admitted input should remain encodable at the private boundary.
	if err != nil {
		return mcpJSONRPCRequest{}, errors.New("MCP tool params could not be encoded")
	}
	return mcpJSONRPCRequest{JSONRPC: "2.0", ID: request.ID, Method: request.Method, Params: encoded}, nil
}

// registerMCPModernToolHandle binds one unguessable public handle to the already-authorized internal child.
func registerMCPModernToolHandle(sess *mcpSession) (string, bool) {
	handle := mcpModernStateHandlePrefix + uuid.NewString()
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	// Registration must lose to revocation, expiry, shutdown, or any earlier child failure.
	if !mcpSessionRegisteredAndActiveLocked(sess) {
		return "", false
	}
	mcpModernToolHandles.Lock()
	mcpModernToolHandles.byHandle[handle] = sess.sessionID
	mcpModernToolHandles.Unlock()
	sess.modernStateHandle = handle
	return handle, true
}

// unregisterMCPModernToolHandle removes only the handle owned by the terminating child session.
func unregisterMCPModernToolHandle(sess *mcpSession) {
	// Compatibility sessions and pre-registration failures have no public modern state.
	if sess == nil || sess.modernStateHandle == "" {
		return
	}
	mcpModernToolHandles.Lock()
	// Exact child identity prevents delayed cleanup from deleting a future mapping after test substitution.
	if mcpModernToolHandles.byHandle[sess.modernStateHandle] == sess.sessionID {
		delete(mcpModernToolHandles.byHandle, sess.modernStateHandle)
	}
	mcpModernToolHandles.Unlock()
}

// resolveMCPModernToolHandle reauthenticates one explicit handle against immutable execution context.
func resolveMCPModernToolHandle(handle, routeID string, admission *mcpModernAdmission, authContext map[string]any) *mcpSession {
	mcpModernToolHandles.RLock()
	sessionID := mcpModernToolHandles.byHandle[handle]
	mcpModernToolHandles.RUnlock()
	// Unknown handles do not reveal whether an earlier runtime existed or expired.
	if sessionID == "" {
		return nil
	}
	sess, ok := lookupMCPSession(sessionID)
	// Public state cannot cross transport, route, promoted app version, token, or connected-resource boundaries.
	if !ok || sess.transport != mcpModernToolTransport || sess.routeID != routeID || sess.appID != admission.target.AppID.String() || sess.tokenID != admission.identity.TokenID || sess.modernStateHandle != handle || !reflect.DeepEqual(sess.authContext, authContext) {
		return nil
	}
	touchMCPSession(sess)
	return sess
}

// adaptMCPModernToolList adds the explicit state contract to execute without changing the compatibility child.
func adaptMCPModernToolList(result map[string]any) error {
	tools, ok := result["tools"].([]any)
	// A successful child catalogue must retain the standard list shape.
	if !ok {
		return errors.New("MCP tool runtime returned an invalid tool catalogue")
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		// Malformed entries cannot be safely rewritten or exposed through modern discovery.
		if !ok {
			return errors.New("MCP tool runtime returned an invalid tool descriptor")
		}
		// search_docs has no state contract and remains byte-shape compatible with the child descriptor.
		if tool["name"] != "execute" {
			continue
		}
		inputSchema, ok := tool["inputSchema"].(map[string]any)
		// Execute must expose an object schema before the adapter can add its explicit handle.
		if !ok {
			return errors.New("MCP execute tool has no valid input schema")
		}
		properties, ok := inputSchema["properties"].(map[string]any)
		// Missing properties would make stateHandle impossible to validate at the client boundary.
		if !ok {
			return errors.New("MCP execute tool has no valid input properties")
		}
		properties[mcpModernStateHandleArgument] = map[string]any{
			"type": "string", "maxLength": 128,
			"description": "Exact state handle returned by an earlier execute call. Supply it only for session state or retained-result continuation.",
		}
		tool["description"] = mcpModernExecuteToolDescription
		tool["outputSchema"] = map[string]any{
			"type": "object", "properties": map[string]any{mcpModernStateHandleArgument: map[string]any{"type": "string"}},
			"required": []string{mcpModernStateHandleArgument}, "additionalProperties": false,
		}
		metadata, _ := tool["_meta"].(map[string]any)
		// The legacy implicit-session extension must not be advertised to a stateless 2026 client.
		if metadata == nil {
			metadata = map[string]any{}
		}
		delete(metadata, "com.usefused/session")
		metadata[mcpModernStateMetadataKey] = map[string]any{
			"schema_version": 1, "mode": "explicit_handle", "input": mcpModernStateHandleArgument,
			"ttl_ms": int(mcpSessionIdleTimeout().Milliseconds()), "automatic_execute_replay": false,
		}
		tool["_meta"] = metadata
	}
	result["ttlMs"] = mcpModernResourceCacheTTL
	result["cacheScope"] = "private"
	return nil
}

// attachMCPModernStateHandle adds explicit continuation state without changing an ordinary provider result body.
func attachMCPModernStateHandle(result map[string]any, handle string) {
	metadata, _ := result["_meta"].(map[string]any)
	// Child execution metadata remains intact beside the modern explicit-state contract.
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[mcpModernStateMetadataKey] = map[string]any{
		"schema_version": 1, "handle": handle, "ttl_ms": int(mcpSessionIdleTimeout().Milliseconds()),
	}
	result["_meta"] = metadata
	result["structuredContent"] = map[string]any{mcpModernStateHandleArgument: handle}
	rewriteMCPModernContinuation(result, handle)
}

// rewriteMCPModernContinuation makes a retained-result next_request directly executable on its owning explicit state.
func rewriteMCPModernContinuation(result map[string]any, handle string) {
	content, ok := result["content"].([]any)
	// Non-text or absent content has no embedded retained-result continuation to adapt.
	if !ok {
		return
	}
	for _, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		// Only JSON text blocks produced by the trusted runtime may carry next_request.
		if !ok || block["type"] != "text" {
			continue
		}
		text, ok := block["text"].(string)
		if !ok {
			continue
		}
		var payload map[string]any
		// Ordinary provider JSON remains exactly unchanged unless it contains the closed continuation shape.
		if json.Unmarshal([]byte(text), &payload) != nil {
			continue
		}
		next, ok := payload["next_request"].(map[string]any)
		if !ok || next["tool"] != "execute" {
			continue
		}
		arguments, ok := next["arguments"].(map[string]any)
		if !ok {
			continue
		}
		arguments[mcpModernStateHandleArgument] = handle
		payload["session"] = map[string]any{"scope": "explicit_state_handle", "state_handle_required": true}
		encoded, err := json.Marshal(payload)
		// The decoded trusted JSON must remain serializable before replacing the original bounded text.
		if err == nil {
			block["text"] = string(encoded)
		}
	}
}

// writeMCPModernChildResponse converts one private child envelope and reports whether a usable result reached the caller.
func writeMCPModernChildResponse(w http.ResponseWriter, requestID json.RawMessage, response string, server FixtureServerMetadata, transform func(map[string]any) error) bool {
	var envelope mcpModernChildEnvelope
	// Child output must be one correlated JSON-RPC 2.0 envelope before any model-visible fields are reused.
	if json.Unmarshal([]byte(response), &envelope) != nil || envelope.JSONRPC != "2.0" || !bytes.Equal(compactJSON(envelope.ID), compactJSON(requestID)) {
		writeMCPModernError(w, requestID, -32603, "MCP tool runtime returned an invalid response", http.StatusBadGateway, nil)
		return false
	}
	// A child protocol error remains correlated but gains the public modern protocol header.
	if envelope.Error != nil {
		status := http.StatusBadRequest
		// Unknown methods are client-addressable misses while internal child failures remain gateway failures.
		if envelope.Error.Code == -32601 {
			status = http.StatusNotFound
		} else if envelope.Error.Code == -32603 {
			status = http.StatusBadGateway
		}
		writeMCPModernError(w, requestID, envelope.Error.Code, envelope.Error.Message, status, envelope.Error.Data)
		return false
	}
	// Missing results cannot be interpreted as successful empty tool output.
	if envelope.Result == nil {
		writeMCPModernError(w, requestID, -32603, "MCP tool runtime returned no result", http.StatusBadGateway, nil)
		return false
	}
	if transform != nil {
		// Adapter transformation is limited to public schema and explicit state metadata.
		if err := transform(envelope.Result); err != nil {
			writeMCPModernError(w, requestID, -32603, err.Error(), http.StatusBadGateway, nil)
			return false
		}
	}
	writeMCPModernResult(w, requestID, envelope.Result, server)
	return true
}

// writeMCPModernRuntimeFailure emits one pre-classified adapter failure unless its limiter already wrote the response.
func writeMCPModernRuntimeFailure(w http.ResponseWriter, requestID json.RawMessage, failure *mcpModernRuntimeFailure) {
	// A nil failure means admission already emitted its exact modern response.
	if failure == nil {
		return
	}
	writeMCPModernError(w, requestID, failure.code, failure.message, failure.status, failure.data)
}

// mcpModernStateUnavailableData gives clients a closed recovery action for expired or context-mismatched handles.
func mcpModernStateUnavailableData() map[string]any {
	return map[string]any{
		"code": "MCP_STATE_HANDLE_UNAVAILABLE", "recovery_action": "create_new_state_handle",
		"execute_request": "reformat_if_state_used", "provider_execution": "not_started", "automatic_replay": false,
	}
}

// mcpModernClientMetadata retains bounded display provenance from per-request metadata without trusting it for authorization.
func mcpModernClientMetadata(r *http.Request, request mcpJSONRPCRequest) mcpsession.Metadata {
	metadata := initialMCPSessionMetadata(r)
	var params struct {
		Meta struct {
			ClientInfo *struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"io.modelcontextprotocol/clientInfo"`
		} `json:"_meta"`
	}
	// Missing, malformed, or oversized self-reported identity remains absent from durable history.
	if json.Unmarshal(request.Params, &params) != nil || params.Meta.ClientInfo == nil {
		return metadata
	}
	candidate := metadata
	candidate.ClientName, candidate.ClientVersion = params.Meta.ClientInfo.Name, params.Meta.ClientInfo.Version
	// Client display claims never influence runtime admission and are retained only when bounded.
	if candidate.Valid() {
		return candidate
	}
	return metadata
}
