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

func mcpStreamableHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sandbox.mcp.streamable_http")
	defer span.End()
	span.SetAttributes(attribute.String("http.request.method", r.Method))
	appID, token, ok := extractMCPParams(w, r)
	if !ok {
		recordMCPStreamableOutcome(span, "unauthorized", true)
		return
	}
	span.SetAttributes(attribute.String("app.id", appID))
	switch r.Method {
	case http.MethodPost:
		handleMCPStreamablePost(ctx, span, w, r, appID, token)
	case http.MethodGet:
		handleMCPStreamableGet(ctx, span, w, r, appID, token)
	case http.MethodDelete:
		handleMCPStreamableDelete(ctx, span, w, r, appID, token)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		recordMCPStreamableOutcome(span, "method_not_allowed", true)
	}
}

// handleMCPStreamablePost admits a bounded envelope before authenticating or starting its runtime session.
func handleMCPStreamablePost(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, appID, token string) {
	body, err := readBoundedMCPMessageBody(w, r)
	// Only a typed size rejection enters limit-specific telemetry; parser failures remain separate.
	if err != nil {
		recordMCPMessageLimit(ctx, appID, mcpStreamableTransport, err)
		recordMCPStreamableOutcome(span, "invalid", true)
		return
	}
	request, err := parseMCPJSONRPCRequest(body)
	if err != nil {
		writeMCPJSONRPCError(w, nil, -32700, "invalid JSON-RPC request", http.StatusBadRequest)
		recordMCPStreamableOutcome(span, "invalid", true)
		return
	}
	sessionID := strings.TrimSpace(r.Header.Get(mcpSessionIDHeader))
	if sessionID == "" {
		handleMCPStreamableInitialize(ctx, span, w, r, appID, token, body, request)
		return
	}
	sess, status, err := authenticateMCPStreamableSession(ctx, appID, token, sessionID, r.Header.Get(mcpProtocolVersionHeader))
	if err != nil {
		writeError(w, status, err.Error())
		recordMCPStreamableOutcome(span, "denied", true)
		return
	}
	if request.Method == "initialize" {
		writeMCPJSONRPCError(w, request.ID, -32600, "session is already initialized", http.StatusBadRequest)
		recordMCPStreamableOutcome(span, "invalid", true)
		return
	}
	if !allowMessage(w, sess.appID) {
		recordMCPStreamableOutcome(span, "rate_limited", true)
		return
	}
	serveMCPStreamableRequest(ctx, span, w, sess, body, request)
}

// handleMCPStreamableInitialize preserves typed admission failures while establishing one authenticated runtime.
func handleMCPStreamableInitialize(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, appID, token string, body []byte, request mcpJSONRPCRequest) {
	// Only a correlated initialize request can establish the transport's immutable session identity.
	if request.Method != "initialize" || len(request.ID) == 0 {
		writeMCPJSONRPCError(w, request.ID, -32600, "initialize is required before creating a session", http.StatusBadRequest)
		recordMCPStreamableOutcome(span, "invalid", true)
		return
	}
	// Admission precedes runtime allocation and provenance retention.
	if !allowMCPStreamableStart(ctx, span, w, appID) {
		return
	}
	protocolVersion, err := initializeMCPProtocolVersion(request.Params)
	// Invalid protocol negotiation cannot establish a session with misleading metadata.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		recordMCPStreamableOutcome(span, "invalid", true)
		return
	}
	authContext, err := mcpSessionAuthContext(r.Header)
	// Connection selectors must validate independently of the display-only client claim.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		recordMCPStreamableOutcome(span, "invalid", true)
		return
	}
	identity, err := validateMCPToken(ctx, appID, token)
	// No network or client attribution is retained before the runtime credential is authorized.
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		recordMCPStreamableOutcome(span, "denied", true)
		return
	}
	sess, err := startMCPStreamableSession(ctx, appID, token, protocolVersion, authContext, identity, initialMCPSessionMetadata(r))
	// Typed catalogue admission codes survive the session-start boundary without authored content.
	if err != nil {
		statusCode, errorCode := mcpSessionStartFailure(err)
		writeError(w, statusCode, errorCode)
		recordMCPStreamableOutcome(span, "start_failed", true)
		return
	}
	w.Header().Set(mcpSessionIDHeader, sess.sessionID)
	w.Header().Set(mcpProtocolVersionHeader, sess.protocolVersion)
	// Failed initialization cannot leave a registered but unusable runtime session behind.
	if !serveMCPStreamableRequest(ctx, span, w, sess, body, request) {
		terminateMCPSession(sess.sessionID, "runtime_failed")
	}
}

func allowMCPStreamableStart(ctx context.Context, span trace.Span, w http.ResponseWriter, appID string) bool {
	if !allowMCPSessionStart(w, appID) {
		recordMCPStreamableOutcome(span, "rate_limited", true)
		return false
	}
	return allowMCPStreamableConcurrency(ctx, span, w)
}

func allowMCPStreamableConcurrency(ctx context.Context, span trace.Span, w http.ResponseWriter) bool {
	limitErr := entitlement.CheckLimit(span, "mcp_sandbox_concurrency", activeMCPSessionCount(), entitlement.LiveEntitlement.Load().MaxSandboxConcurrency)
	if limitErr == nil {
		return true
	}
	writeError(w, http.StatusPaymentRequired, limitErr.Error())
	recordMCPStreamableOutcome(span, "concurrency_limited", true)
	return false
}

// startMCPStreamableSession starts one isolated runtime that survives active use until authorization or cleanup ends it.
func startMCPStreamableSession(ctx context.Context, appID, token, protocolVersion string, authContext map[string]any, identity auth.RuntimeIdentity, metadata mcpsession.Metadata) (*mcpSession, error) {
	// Runtime state is loaded only after the shared token authorization has succeeded.
	if err := globalObjectCache.ConnectSDK(ctx, appID); err != nil {
		return nil, err
	}
	sessionID := uuid.NewString()
	runtimeCtx, cancel := mcpSessionContext(context.WithoutCancel(ctx), identity.TokenPolicy.ExpiresAt)
	fixture, err := prepareSessionFixture(runtimeCtx, appID, identity.TokenPolicy)
	// Failed preparation must release the exact app's connection ownership.
	if err != nil {
		cleanupFailedMCPStreamableStart(appID, sessionID, cancel)
		return nil, err
	}
	cmd, err := buildMCPCommand(runtimeCtx, sessionID, fixture)
	// No partially prepared process survives an invalid runtime launch request.
	if err != nil {
		cleanupFailedMCPStreamableStart(appID, sessionID, cancel)
		return nil, err
	}
	stdin, stdout, err := setupPipesAndStart(cmd)
	// A failed process start leaves no usable session to register.
	if err != nil {
		cleanupFailedMCPStreamableStart(appID, sessionID, cancel)
		return nil, err
	}
	sess := &mcpSession{
		clientMetadata: metadata,
		appID:          appID, sessionID: sessionID, tokenID: identity.TokenID,
		protocolVersion: protocolVersion, transport: mcpStreamableTransport,
		cmd: cmd, stdin: stdin, cancel: cancel, responses: make(chan string, 32),
		token: token, fixture: fixture, authContext: authContext,
	}
	registerMCPSession(runtimeCtx, sess)
	go pumpMCPStreamableResponses(runtimeCtx, sess, stdout)
	return sess, nil
}

func cleanupFailedMCPStreamableStart(appID, sessionID string, cancel context.CancelFunc) {
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
	touchMCPSession(sess)
	callID := trackMCPToolCall(ctx, request, sess)
	// Dispatch failure ends both pending-call and search state before session cleanup.
	if _, err := sess.stdin.Write(append(body, '\n')); err != nil {
		completeMCPToolCall(sess, callID, "", "dispatch_failed")
		writeMCPJSONRPCError(w, request.ID, -32603, "failed to dispatch request", http.StatusBadGateway)
		recordMCPStreamableOutcome(span, "dispatch_failed", true)
		terminateMCPSession(sess.sessionID, "runtime_failed")
		return false
	}
	// Notifications are accepted without waiting for a response envelope.
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		recordMCPStreamableOutcome(span, "accepted", false)
		return true
	}
	response, err := waitForMCPStreamableResponse(ctx, sess, request.ID)
	// Transport failure is collapsed before it reaches telemetry or the public response.
	if err != nil {
		failureCode := "runtime_unavailable"
		// Deadline ownership is safe to distinguish without attaching raw errors.
		if errors.Is(err, context.DeadlineExceeded) {
			failureCode = "tool_call_timeout"
		}
		completeMCPToolCall(sess, callID, "", failureCode)
		writeMCPJSONRPCError(w, request.ID, -32603, "MCP runtime did not respond", http.StatusBadGateway)
		recordMCPStreamableOutcome(span, "runtime_failed", true)
		terminateMCPSession(sess.sessionID, "runtime_failed")
		return false
	}
	completeMCPToolCall(sess, callID, response, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, response)
	recordMCPStreamableOutcome(span, "success", false)
	return true
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

func compactJSON(value json.RawMessage) []byte {
	var buffer bytes.Buffer
	if json.Compact(&buffer, value) != nil {
		return value
	}
	return buffer.Bytes()
}

// handleMCPStreamableGet ties keepalives to session cancellation without imposing a fixed age on active sessions.
func handleMCPStreamableGet(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, appID, token string) {
	sess, status, err := authenticateMCPStreamableSession(ctx, appID, token, r.Header.Get(mcpSessionIDHeader), r.Header.Get(mcpProtocolVersionHeader))
	if err != nil {
		writeError(w, status, err.Error())
		recordMCPStreamableOutcome(span, "denied", true)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		recordMCPStreamableOutcome(span, "unsupported", true)
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
	recordMCPStreamableOutcome(span, "closed", false)
}

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

func handleMCPStreamableDelete(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, appID, token string) {
	sess, status, err := authenticateMCPStreamableSession(ctx, appID, token, r.Header.Get(mcpSessionIDHeader), r.Header.Get(mcpProtocolVersionHeader))
	if err != nil {
		writeError(w, status, err.Error())
		recordMCPStreamableOutcome(span, "denied", true)
		return
	}
	touchMCPSession(sess)
	terminateMCPSession(sess.sessionID, "client_terminated")
	w.WriteHeader(http.StatusNoContent)
	recordMCPStreamableOutcome(span, "deleted", false)
}

// authenticateMCPStreamableSession refreshes activity only after session ownership and token policy succeed.
func authenticateMCPStreamableSession(ctx context.Context, appID, token, sessionID, protocolVersion string) (*mcpSession, int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, http.StatusBadRequest, errors.New("Mcp-Session-Id header is required")
	}
	sess, ok := lookupMCPSession(sessionID)
	if !ok || sess.transport != mcpStreamableTransport || sess.appID != appID {
		return nil, http.StatusNotFound, errors.New("mcp session not found or expired")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sess.token)) != 1 {
		return nil, http.StatusUnauthorized, errors.New("invalid Authorization header")
	}
	identity, err := validateMCPToken(ctx, appID, token)
	if err != nil || identity.TokenID != sess.tokenID {
		return nil, http.StatusUnauthorized, errors.New("invalid token")
	}
	if protocolVersion != "" && protocolVersion != sess.protocolVersion {
		return nil, http.StatusBadRequest, errors.New("MCP-Protocol-Version does not match the session")
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

func initializeMCPProtocolVersion(params json.RawMessage) (string, error) {
	var initialize mcpInitializeParams
	if json.Unmarshal(params, &initialize) != nil {
		return "", errors.New("initialize params are invalid")
	}
	version := strings.TrimSpace(initialize.ProtocolVersion)
	if version == "" || len(version) > 32 || strings.ContainsAny(version, " \t\r\n") {
		return "", errors.New("initialize protocolVersion must be a non-empty token of at most 32 characters")
	}
	return version, nil
}

func writeMCPJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string, status int) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

func recordMCPStreamableOutcome(span trace.Span, outcome string, failed bool) {
	span.SetAttributes(attribute.String("outcome", outcome))
	if failed {
		span.SetStatus(codes.Error, outcome)
	}
}
