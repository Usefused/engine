package sandbox

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/mcpsession"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	mcpSessionIDHeader       = "Mcp-Session-Id"
	mcpProtocolVersionHeader = "MCP-Protocol-Version"
	mcpStreamableTransport   = "streamable_http"
)

type mcpJSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// mcpStreamableHandler owns one audited HTTP transport request without exposing its session identity.
func mcpStreamableHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sandbox.mcp.streamable_http")
	defer span.End()
	span.SetAttributes(attribute.String("http.request.method", r.Method))
	// Origin admission runs before bearer parsing and transport-state lookup.
	if !admitMCPRequestOrigin(w, r) {
		recordMCPTransportOutcome(span, "origin_denied", true)
		return
	}
	routeID, token, ok := extractMCPParams(w, r)
	if !ok {
		recordMCPTransportOutcome(span, "unauthorized", true)
		return
	}
	switch r.Method {
	case http.MethodPost:
		handleMCPStreamablePost(ctx, span, w, r, routeID, token)
	case http.MethodGet:
		handleMCPStreamableGet(ctx, span, w, r, routeID, token)
	case http.MethodDelete:
		handleMCPStreamableDelete(ctx, span, w, r, routeID, token)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		recordMCPTransportOutcome(span, "method_not_allowed", true)
	}
}

// handleMCPStreamablePost admits a bounded envelope before authenticating or starting its runtime session.
func handleMCPStreamablePost(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string) {
	body, err := readBoundedMCPMessageBody(w, r)
	// Only a typed size rejection enters limit-specific telemetry; parser failures remain separate.
	if err != nil {
		recordMCPMessageLimit(ctx, routeID, mcpStreamableTransport, err)
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	request, err := parseMCPJSONRPCRequest(body)
	if err != nil {
		writeMCPJSONRPCError(w, nil, -32700, "invalid JSON-RPC request", http.StatusBadRequest)
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	sessionID := strings.TrimSpace(r.Header.Get(mcpSessionIDHeader))
	// Only initialize may create a transport session; every other first request receives an exact recovery contract.
	if sessionID == "" {
		handleMCPStreamableInitialize(ctx, span, w, r, routeID, token, body, request)
		return
	}
	sess, status, err := authenticateMCPStreamableSession(ctx, routeID, token, sessionID, r.Header.Get(mcpProtocolVersionHeader))
	if err != nil {
		// Reviewed session states carry exact client recovery; credential failures retain their opaque denial.
		if failure, typed := mcpSessionAuthenticationFailure(err); typed {
			writeMCPJSONRPCSessionError(w, request.ID, err.Error(), status, failure)
			recordMCPTransportFailure(span, "denied", failure)
			return
		}
		writeMCPJSONRPCError(w, request.ID, -32600, err.Error(), status)
		recordMCPTransportOutcome(span, "denied", true)
		return
	}
	if request.Method == "initialize" {
		writeMCPJSONRPCError(w, request.ID, -32600, "session is already initialized", http.StatusBadRequest)
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	if !allowMessage(w, sess.appID) {
		recordMCPTransportOutcome(span, "rate_limited", true)
		return
	}
	serveMCPStreamableRequest(ctx, span, w, sess, body, request)
}

// handleMCPStreamableInitialize preserves typed admission failures while establishing one authenticated runtime.
func handleMCPStreamableInitialize(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string, body []byte, request mcpJSONRPCRequest) {
	// Request-shape admission owns its typed failure before any route lookup.
	if !admitMCPInitializeRequest(span, w, request) {
		return
	}
	target, admitted := admitMCPStreamableRoute(ctx, span, w, routeID, request.ID)
	// Route admission owns its bounded client error and telemetry outcome.
	if !admitted {
		return
	}
	appID := target.AppID.String()
	span.SetAttributes(
		attribute.String("app.id", appID),
		attribute.String("app.family_id", target.AppFamilyID.String()),
		attribute.Bool("mcp.route.stable", target.Stable),
	)
	// Admission precedes runtime allocation and provenance retention.
	if !allowMCPStreamableStart(ctx, span, w, appID) {
		return
	}
	protocolVersion, err := initializeMCPProtocolVersion(request.Params)
	// Invalid protocol negotiation cannot establish a session with misleading metadata.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	authContext, err := mcpSessionAuthContext(r.Header)
	// Connection selectors must validate independently of the display-only client claim.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	identity, err := validateMCPToken(ctx, appID, token)
	// No network or client attribution is retained before the runtime credential is authorized.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32600, "invalid token", http.StatusUnauthorized)
		recordMCPTransportOutcome(span, "denied", true)
		return
	}
	sess, err := startMCPStreamableSession(ctx, routeID, appID, token, protocolVersion, authContext, identity, initialMCPSessionMetadata(r))
	// Typed catalogue admission codes survive the session-start boundary without authored content.
	if err != nil {
		statusCode, errorCode := mcpSessionStartFailure(err)
		failure := mcpSessionStartFailureData(errorCode)
		writeMCPJSONRPCSessionError(w, request.ID, "MCP session could not start", statusCode, failure)
		recordMCPTransportFailure(span, "start_failed", failure)
		return
	}
	// Session headers are published only after the child returns a successful negotiated InitializeResult.
	if !serveMCPStreamableRequest(ctx, span, w, sess, body, request) {
		terminateMCPSession(sess.sessionID, "runtime_failed")
	}
}

// admitMCPInitializeRequest requires one correlated initialize request before
// stable-route resolution or runtime allocation can occur.
func admitMCPInitializeRequest(span trace.Span, w http.ResponseWriter, request mcpJSONRPCRequest) bool {
	// Notifications and null IDs cannot establish client-owned transport state.
	if request.Method != "initialize" || len(request.ID) == 0 || string(compactJSON(request.ID)) == "null" {
		failure := mcpInitializeRequiredFailure()
		writeMCPJSONRPCErrorData(w, request.ID, -32600, "initialize is required before creating a session", http.StatusBadRequest, failure)
		recordMCPTransportFailure(span, "invalid", failure)
		return false
	}
	return true
}

// admitMCPStreamableRoute snapshots the stable target for a new session and
// keeps route lifecycle state opaque on every admission failure.
func admitMCPStreamableRoute(ctx context.Context, span trace.Span, w http.ResponseWriter, routeID string, requestID json.RawMessage) (*store.MCPRouteTarget, bool) {
	target, err := resolveMCPRoute(ctx, routeID)
	// Stable and pinned route failures share the token denial contract so
	// lifecycle state cannot be enumerated before authentication.
	if err != nil {
		writeMCPJSONRPCError(w, requestID, -32600, "invalid token", http.StatusUnauthorized)
		recordMCPTransportOutcome(span, "denied", true)
		return nil, false
	}
	return target, true
}

// allowMCPStreamableStart applies rate and concurrency admission before allocating a child runtime.
func allowMCPStreamableStart(ctx context.Context, span trace.Span, w http.ResponseWriter, appID string) bool {
	if !allowMCPSessionStart(w, appID) {
		recordMCPTransportOutcome(span, "rate_limited", true)
		return false
	}
	return allowMCPStreamableConcurrency(ctx, span, w)
}

// allowMCPStreamableConcurrency keeps entitlement failure on the existing handshake span.
func allowMCPStreamableConcurrency(ctx context.Context, span trace.Span, w http.ResponseWriter) bool {
	limitErr := entitlement.CheckLimit(span, "mcp_sandbox_concurrency", activeMCPSessionCount(), entitlement.LiveEntitlement.Load().MaxSandboxConcurrency)
	if limitErr == nil {
		return true
	}
	writeError(w, http.StatusPaymentRequired, limitErr.Error())
	recordMCPTransportOutcome(span, "concurrency_limited", true)
	return false
}

// startMCPStreamableSession starts one isolated runtime that survives active use until authorization or cleanup ends it.
func startMCPStreamableSession(ctx context.Context, routeID, appID, token, protocolVersion string, authContext map[string]any, identity auth.RuntimeIdentity, metadata mcpsession.Metadata) (*mcpSession, error) {
	// Runtime state is loaded only after the shared token authorization has succeeded.
	if err := globalObjectCache.ConnectSDK(ctx, appID); err != nil {
		return nil, err
	}
	sessionID := uuid.NewString()
	runtimeCtx, cancel := mcpSessionContext(context.WithoutCancel(ctx), identity.TokenPolicy.ExpiresAt)
	fixture, err := prepareSessionFixture(runtimeCtx, appID, identity.TokenPolicy)
	// Failed preparation must release the exact app's connection ownership.
	if err != nil {
		cleanupFailedMCPSessionStart(appID, sessionID, cancel)
		return nil, err
	}
	cmd, err := buildMCPCommand(runtimeCtx, sessionID, fixture)
	// No partially prepared process survives an invalid runtime launch request.
	if err != nil {
		cleanupFailedMCPSessionStart(appID, sessionID, cancel)
		return nil, err
	}
	stdin, stdout, err := setupPipesAndStart(cmd)
	// A failed process start leaves no usable session to register.
	if err != nil {
		cleanupFailedMCPSessionStart(appID, sessionID, cancel)
		return nil, err
	}
	sess := &mcpSession{
		clientMetadata: metadata,
		appID:          appID, routeID: routeID, sessionID: sessionID, tokenID: identity.TokenID,
		protocolVersion: protocolVersion, transport: mcpStreamableTransport,
		cmd: cmd, stdin: stdin, cancel: cancel, responses: make(chan string, 32),
		token: token, fixture: fixture, authContext: authContext,
	}
	registerMCPSession(runtimeCtx, sess)
	go pumpMCPStreamableResponses(runtimeCtx, sess, stdout)
	return sess, nil
}

// cleanupFailedMCPSessionStart releases shared cache and filesystem ownership before registration succeeds.
func cleanupFailedMCPSessionStart(appID, sessionID string, cancel context.CancelFunc) {
	cancel()
	os.RemoveAll(mcpSessionTmpDir(sessionID))
	globalObjectCache.DisconnectSDK(appID)
}

// pumpMCPStreamableResponses shares the bounded cancelable scanner used by legacy SSE.
func pumpMCPStreamableResponses(ctx context.Context, sess *mcpSession, stdout io.ReadCloser) {
	defer close(sess.responses)
	_ = scanMCPResponseLines(ctx, stdout, sess.responses)
	terminateMCPSession(sess.sessionID, "runtime_failed")
}

// serveMCPStreamableRequest serializes one child exchange and closes its bounded tool observation.
func serveMCPStreamableRequest(ctx context.Context, span trace.Span, w http.ResponseWriter, sess *mcpSession, body []byte, request mcpJSONRPCRequest) bool {
	sess.requestMu.Lock()
	defer sess.requestMu.Unlock()
	callID, written, err, active := writeMCPChildRequestLocked(ctx, sess, body, request)
	// A request queued behind termination must not reach a retired provider-capable runtime.
	if !active {
		failure := mcpUnavailableSessionFailure()
		writeMCPJSONRPCSessionError(w, request.ID, errMCPSessionUnavailable.Error(), http.StatusNotFound, failure)
		recordMCPTransportFailure(span, "denied", failure)
		return false
	}
	// Dispatch failure ends both pending-call and search state before session cleanup.
	if err != nil {
		failure := mcpDispatchFailure(request, written)
		completeMCPToolCall(sess, callID, "", "dispatch_failed")
		writeMCPJSONRPCSessionError(w, request.ID, "MCP runtime request dispatch failed", http.StatusBadGateway, failure)
		recordMCPTransportFailure(span, "dispatch_failed", failure)
		terminateMCPSession(sess.sessionID, "runtime_failed")
		return false
	}
	// Notifications are accepted without waiting for a response envelope.
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		recordMCPTransportOutcome(span, "accepted", false)
		return true
	}
	response, err := waitForMCPStreamableResponse(ctx, sess, request.ID)
	// Missing child output receives bounded recovery metadata without exposing the raw transport error.
	if err != nil {
		failureCode := "runtime_unavailable"
		// Deadline ownership is safe to distinguish without attaching raw errors.
		if errors.Is(err, context.DeadlineExceeded) {
			failureCode = "tool_call_timeout"
		}
		failure := mcpRuntimeFailureForRequest(request, mcpRuntimeResponseFailedCode, "runtime_response", "dispatched")
		completeMCPToolCall(sess, callID, "", failureCode)
		writeMCPJSONRPCSessionError(w, request.ID, "MCP runtime response unavailable", http.StatusBadGateway, failure)
		recordMCPTransportFailure(span, "runtime_failed", failure)
		terminateMCPSession(sess.sessionID, "runtime_failed")
		return false
	}
	completeMCPToolCall(sess, callID, response, "")
	return writeMCPStreamableRuntimeResponse(span, w, sess, request, response)
}

// writeMCPStreamableRuntimeResponse commits initialization and verifies final HTTP delivery.
func writeMCPStreamableRuntimeResponse(span trace.Span, w http.ResponseWriter, sess *mcpSession, request mcpJSONRPCRequest, response string) bool {
	// Initialization must adopt the child SDK's negotiated version before any session header becomes authoritative.
	if request.Method == "initialize" {
		protocolVersion, valid := mcpInitializeResultProtocolVersion(response)
		// Invalid negotiation is read-only runtime failure, never evidence that a provider mutation ran.
		if !valid {
			failure := mcpRuntimeFailureForRequest(request, mcpRuntimeResponseFailedCode, "session_initialization", "dispatched")
			writeMCPJSONRPCSessionError(w, request.ID, "MCP runtime returned an invalid initialize result", http.StatusBadGateway, failure)
			recordMCPTransportFailure(span, "runtime_failed", failure)
			return false
		}
		// Negotiation commits only while this exact session remains active, preventing a stale header after revocation.
		if !commitMCPStreamableInitialize(sess, protocolVersion) {
			failure := mcpUnavailableSessionFailure()
			writeMCPJSONRPCSessionError(w, request.ID, errMCPSessionUnavailable.Error(), http.StatusNotFound, failure)
			recordMCPTransportFailure(span, "denied", failure)
			return false
		}
		w.Header().Set(mcpSessionIDHeader, sess.sessionID)
		w.Header().Set(mcpProtocolVersionHeader, protocolVersion)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	written, writeErr := io.WriteString(w, response)
	// A successful status with a truncated body still leaves the client without a trustworthy request outcome.
	if writeErr == nil && written != len(response) {
		writeErr = io.ErrShortWrite
	}
	// Delivery failure retires the session so an unreceived initialize header cannot leave an orphaned runtime.
	if writeErr != nil {
		failure := mcpRuntimeFailureForRequest(request, mcpRuntimeResponseFailedCode, "transport_response_delivery", "dispatched")
		recordMCPTransportFailure(span, "response_delivery_failed", failure)
		terminateMCPSession(sess.sessionID, "transport_failed")
		return false
	}
	recordMCPTransportOutcome(span, "success", false)
	return true
}

// commitMCPStreamableInitialize atomically orders negotiated state and its durable event before termination.
func commitMCPStreamableInitialize(sess *mcpSession, protocolVersion string) bool {
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	// Only the exact still-registered session may publish negotiated metadata or authorize response headers.
	if !mcpSessionRegisteredAndActiveLocked(sess) {
		return false
	}
	return commitMCPInitializationLocked(sess, protocolVersion)
}

func waitForMCPStreamableResponse(ctx context.Context, sess *mcpSession, requestID json.RawMessage) (string, error) {
	timer := time.NewTimer(time.Duration(cfg.Sandbox.ToolCallTimeoutSeconds) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", context.DeadlineExceeded
		case response, ok := <-sess.responses:
			if !ok {
				return "", io.EOF
			}
			if mcpJSONRPCResponseMatches(response, requestID) {
				return response, nil
			}
		}
	}
}

func mcpJSONRPCResponseMatches(response string, requestID json.RawMessage) bool {
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal([]byte(response), &envelope) != nil || len(envelope.ID) == 0 {
		return false
	}
	return bytes.Equal(compactJSON(envelope.ID), compactJSON(requestID))
}

// compactJSON normalizes one admitted envelope before forwarding it to the
// newline-delimited child protocol.
func compactJSON(value json.RawMessage) []byte {
	var buffer bytes.Buffer
	if json.Compact(&buffer, value) != nil {
		return value
	}
	return buffer.Bytes()
}

// handleMCPStreamableGet ties keepalives to session cancellation without imposing a fixed age on active sessions.
func handleMCPStreamableGet(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string) {
	sess, status, err := authenticateMCPStreamableSession(ctx, routeID, token, r.Header.Get(mcpSessionIDHeader), r.Header.Get(mcpProtocolVersionHeader))
	if err != nil {
		// GET has no JSON-RPC ID, so reviewed session failures use typed HTTP recovery data.
		if failure, typed := mcpSessionAuthenticationFailure(err); typed {
			writeMCPSessionHTTPError(w, status, err.Error(), failure)
			recordMCPTransportFailure(span, "denied", failure)
			return
		}
		writeError(w, status, err.Error())
		recordMCPTransportOutcome(span, "denied", true)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		recordMCPTransportOutcome(span, "unsupported", true)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	touchMCPSession(sess)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	streamCtx, cancel := mcpSessionRequestContext(ctx, sess)
	defer cancel()
	streamMCPKeepAlives(streamCtx, w, flusher, sess.sessionID)
	recordMCPTransportOutcome(span, "closed", false)
}

// streamMCPKeepAlives keeps an attached response open without treating
// server-originated traffic as client session activity.
func streamMCPKeepAlives(ctx context.Context, w io.Writer, flusher http.Flusher, sessionID string) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, ok := lookupMCPSession(sessionID); !ok {
				return
			}
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleMCPStreamableDelete ends only the authenticated transport session and preserves the reusable token.
func handleMCPStreamableDelete(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string) {
	sess, status, err := authenticateMCPStreamableSession(ctx, routeID, token, r.Header.Get(mcpSessionIDHeader), r.Header.Get(mcpProtocolVersionHeader))
	if err != nil {
		// DELETE has no JSON-RPC ID, so reviewed session failures use typed HTTP recovery data.
		if failure, typed := mcpSessionAuthenticationFailure(err); typed {
			writeMCPSessionHTTPError(w, status, err.Error(), failure)
			recordMCPTransportFailure(span, "denied", failure)
			return
		}
		writeError(w, status, err.Error())
		recordMCPTransportOutcome(span, "denied", true)
		return
	}
	touchMCPSession(sess)
	terminateMCPSession(sess.sessionID, "client_terminated")
	w.WriteHeader(http.StatusNoContent)
	recordMCPTransportOutcome(span, "deleted", false)
}

// authenticateMCPStreamableSession refreshes activity only after session ownership and token policy succeed.
func authenticateMCPStreamableSession(ctx context.Context, routeID, token, sessionID, protocolVersion string) (*mcpSession, int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, http.StatusBadRequest, errMCPSessionHeaderRequired
	}
	sess, ok := lookupMCPSession(sessionID)
	// Route ownership is retained at initialization so promoting a family URL
	// does not move an in-flight session to a different immutable runtime.
	if !ok || sess.transport != mcpStreamableTransport || sess.routeID != routeID {
		return nil, http.StatusNotFound, errMCPSessionUnavailable
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sess.token)) != 1 {
		return nil, http.StatusUnauthorized, errors.New("invalid Authorization header")
	}
	identity, err := validateMCPToken(ctx, sess.appID, token)
	if err != nil || identity.TokenID != sess.tokenID {
		return nil, http.StatusUnauthorized, errors.New("invalid token")
	}
	// Negotiated state is read under the same lock used by initialization and lifecycle publication.
	if protocolVersion != "" && protocolVersion != mcpSessionProtocolVersion(sess) {
		return nil, http.StatusBadRequest, errMCPSessionProtocolMismatch
	}
	touchMCPSession(sess)
	return sess, http.StatusOK, nil
}

func parseMCPJSONRPCRequest(body []byte) (mcpJSONRPCRequest, error) {
	var request mcpJSONRPCRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return mcpJSONRPCRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return mcpJSONRPCRequest{}, errors.New("request must contain one JSON document")
	}
	if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
		return mcpJSONRPCRequest{}, errors.New("invalid JSON-RPC envelope")
	}
	return request, nil
}

// initializeMCPProtocolVersion validates the client token before the child SDK performs supported-version negotiation.
func initializeMCPProtocolVersion(params json.RawMessage) (string, error) {
	var initialize mcpInitializeParams
	if json.Unmarshal(params, &initialize) != nil {
		return "", errors.New("initialize params are invalid")
	}
	version := strings.TrimSpace(initialize.ProtocolVersion)
	// A bounded token can be forwarded to the child for normal MCP negotiation without header ambiguity.
	if !validMCPProtocolVersionToken(version) {
		return "", errors.New("initialize protocolVersion must be a non-empty token of at most 32 characters")
	}
	return version, nil
}

// validMCPProtocolVersionToken admits only the portable header grammar shared by requests and child results.
func validMCPProtocolVersionToken(version string) bool {
	return version != "" && len(version) <= 32 && !strings.ContainsAny(version, " \t\r\n")
}

// mcpInitializeResultProtocolVersion extracts only a successful child negotiation result.
func mcpInitializeResultProtocolVersion(response string) (string, bool) {
	var envelope struct {
		JSONRPC string `json:"jsonrpc"`
		Result  *struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	// An error or malformed child result cannot establish a usable transport session.
	if json.Unmarshal([]byte(response), &envelope) != nil || envelope.JSONRPC != "2.0" || envelope.Result == nil || len(envelope.Error) != 0 {
		return "", false
	}
	version := strings.TrimSpace(envelope.Result.ProtocolVersion)
	return version, validMCPProtocolVersionToken(version)
}

// writeMCPJSONRPCError retains the established envelope for failures without additional recovery data.
func writeMCPJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string, status int) {
	writeMCPJSONRPCErrorData(w, id, code, message, status, nil)
}

// writeMCPJSONRPCErrorData serializes optional trusted recovery metadata without string interpolation.
func writeMCPJSONRPCErrorData(w http.ResponseWriter, id json.RawMessage, code int, message string, status int, data any) {
	envelope := mcpJSONRPCErrorEnvelope(id, code, message, data)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// mcpJSONRPCErrorEnvelope builds the shared correlated error shape for HTTP and legacy SSE delivery.
func mcpJSONRPCErrorEnvelope(id json.RawMessage, code int, message string, data any) map[string]any {
	// Notifications and parse failures have no request identifier to correlate.
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	errorBody := map[string]any{"code": code, "message": message}
	// Ordinary protocol failures retain their compact historical shape.
	if data != nil {
		errorBody["data"] = data
	}
	return map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": errorBody,
	}
}

// recordMCPTransportOutcome keeps all MCP HTTP transport spans on one bounded outcome vocabulary.
func recordMCPTransportOutcome(span trace.Span, outcome string, failed bool) {
	span.SetAttributes(attribute.String("outcome", outcome))
	// Failed HTTP transport work is an OTEL error without attaching raw response or request content.
	if failed {
		span.SetStatus(codes.Error, outcome)
	}
}
