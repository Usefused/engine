package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/runtime"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

const maxMCPMessageBodyBytes = 256 * 1024

// mcpSseHandler handles SSE connections for MCP sessions.
//
// Isolation guarantee: each connection spawns a brand-new `node` process.
// No memory, globals, or file handles are shared between sessions.
func mcpSseHandler(w http.ResponseWriter, r *http.Request) {
	appIDHex, token, ok := extractMCPParams(w, r)
	if !ok {
		return
	}
	authContext, err := mcpSessionAuthContext(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !allowMCPSessionStart(w, appIDHex) {
		return
	}

	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sandbox.mcp.concurrency_check")
	if limitErr := entitlement.CheckLimit(span, "mcp_sandbox_concurrency", activeMCPSessionCount(), entitlement.LiveEntitlement.Load().MaxSandboxConcurrency); limitErr != nil {
		slog.InfoContext(ctx, "mcp sse denied: max sandbox concurrency reached", "limit", limitErr.Limit, "current", limitErr.Current)
		writeError(w, http.StatusPaymentRequired, limitErr.Error())
		span.End()
		return
	}
	span.End()

	identity, connected := connectMCPApp(w, r.Context(), appIDHex, token)
	if !connected {
		return
	}
	sessionID := uuid.NewString()
	ctx, cancel := mcpSessionContext(r.Context(), identity.TokenPolicy.ExpiresAt)
	fixture, err := prepareSessionFixture(ctx, appIDHex, identity.TokenPolicy)
	if err != nil {
		cancel()
		globalObjectCache.DisconnectSDK(appIDHex)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to build mcp fixture: %v", err))
		return
	}
	cmd, err := buildMCPCommand(ctx, sessionID, fixture)
	if err != nil {
		cancel()
		globalObjectCache.DisconnectSDK(appIDHex)
		writeError(w, http.StatusInternalServerError, "failed to prepare mcp runtime")
		return
	}
	stdin, stdout, err := setupPipesAndStart(cmd)
	if err != nil {
		cancel()
		globalObjectCache.DisconnectSDK(appIDHex)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	registerMCPSession(ctx, &mcpSession{
		appID: appIDHex, sessionID: sessionID, tokenID: identity.TokenID,
		protocolVersion: "2024-11-05", transport: "sse", cmd: cmd, stdin: stdin,
		cancel: cancel, token: token,
		fixture: fixture, authContext: authContext,
	})
	defer terminateMCPSession(sessionID, "client_disconnected")

	flusher, ok := setupSSEResponse(w, sessionID)
	if !ok {
		return
	}
	processMCPStream(ctx, w, flusher, stdout, sessionID)
}

func connectMCPApp(w http.ResponseWriter, ctx context.Context, appID, token string) (auth.RuntimeIdentity, bool) {
	identity, err := validateMCPToken(ctx, appID, token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return auth.RuntimeIdentity{}, false
	}
	// The execution token authorizes this boundary only. Cache loading receives
	// no credential value because Registry access uses Engine identity.
	if err := globalObjectCache.ConnectSDK(ctx, appID); err != nil {
		writeError(w, http.StatusUnauthorized, fmt.Sprintf("failed to handshake app cache: %v", err))
		return auth.RuntimeIdentity{}, false
	}
	return identity, true
}

// extractMCPParams extracts and validates required URL parameters and headers.
func extractMCPParams(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	appIDHex := chi.URLParam(r, "id")
	if appIDHex == "" {
		writeError(w, http.StatusBadRequest, "app id required")
		return "", "", false
	}

	token, ok := extractBearerAuthToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "Authorization header required")
		return "", "", false
	}
	return appIDHex, token, true
}

func validateMCPToken(ctx context.Context, appIDHex, token string) (auth.RuntimeIdentity, error) {
	if globalTokenValidator == nil {
		return auth.RuntimeIdentity{}, auth.ErrUnauthorized
	}
	appID, err := uuid.Parse(appIDHex)
	if err != nil {
		return auth.RuntimeIdentity{}, err
	}
	// Every session passes through the process-shared validator. Valid entries
	// are bounded to 30 seconds and precise revoke events evict them immediately.
	return globalTokenValidator.Validate(ctx, appID, token)
}

func mcpSessionContext(parent context.Context, expiresAt *time.Time) (context.Context, context.CancelFunc) {
	if expiresAt == nil {
		return context.WithCancel(parent)
	}
	// A token deadline closes the child runtime as well as future dispatches,
	// so expired credentials cannot leave a discoverable MCP session behind.
	return context.WithDeadline(parent, *expiresAt)
}

func extractBearerAuthToken(authHeader string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	return strings.TrimSpace(token), ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != ""
}

// buildMCPCommand constructs the Node.js process for this MCP session.
//
// Security controls applied here (code-level, platform-independent):
//
//  1. Memory cap via NODE_OPTIONS (64 MB heap, 4 MB semi-space).
//  2. HOME and TMPDIR are redirected so the process cannot write to the
//     user home directory. TMPDIR is scoped to a per-session directory
//     that is cleaned up when the MCP session terminates.
//  3. applySysProcAttr sets Pdeathsig+Setpgid on Linux (no-op on macOS).
//
// The fixed runtime receives only a session identifier, Engine port, and
// scoped operation fixture. Provider credentials never enter the child
// process or its environment.
func buildMCPCommand(ctx context.Context, sessionID string, fixture *Fixture) (*exec.Cmd, error) {
	// Per-session storage keeps operation catalogs isolated between tenants.
	sessionTmpDir := mcpSessionTmpDir(sessionID)
	if err := os.MkdirAll(sessionTmpDir, 0700); err != nil {
		return nil, err
	}
	fixturePath, err := writeSessionFixture(sessionTmpDir, fixture)
	if err != nil {
		return nil, err
	}

	entrypoint := sharedRuntimeEntrypointPath()
	if entrypoint == "" {
		entrypoint = filepath.Join(sessionTmpDir, "bundle.js")
		if err := os.WriteFile(entrypoint, runtime.MCPSharedRuntimeBundle, 0600); err != nil {
			return nil, fmt.Errorf("write mcp bundle: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, "node", entrypoint)

	cmd.Env = []string{
		// Memory limits — keep the Node process lean.
		"NODE_OPTIONS=--max-old-space-size=64 --max-semi-space-size=4",
		"PATH=" + os.Getenv("PATH"),
		// FS isolation: redirect home and tmp writes to controlled locations.
		"HOME=/dev/null",
		"TMPDIR=" + sessionTmpDir,
		"FUSED_SESSION_ID=" + sessionID,
		"FUSED_ENGINE_PORT=" + globalEnginePort,
		"FUSED_FIXTURE_PATH=" + fixturePath,
	}

	cmd.Stderr = os.Stderr

	// Linux: kill child if parent dies; place child in own process group.
	applySysProcAttr(cmd)

	return cmd, nil
}

// sharedRuntimeEntrypointPath resolves the compiled shared MCP runtime's
// entrypoint, overridable via FUSED_SHARED_RUNTIME_PATH (mirrors
// sharedRuntimeFixturePath in sandbox.go for the same reason: deployments
// and tests don't all run with cwd == repo root).
func sharedRuntimeEntrypointPath() string {
	if p := os.Getenv("FUSED_SHARED_RUNTIME_PATH"); p != "" {
		return p
	}
	return ""
}

// prepareSessionFixture derives discovery from the exact authorized app scope.
// Keeping this outside the handler preserves one failure boundary for missing
// or incomplete immutable snapshots.
func prepareSessionFixture(ctx context.Context, appIDHex string, policy store.AppTokenPolicy) (*Fixture, error) {
	selections, err := validateAndParseScope(ctx, globalObjectCache, appIDHex)
	if err != nil {
		return nil, fmt.Errorf("load app scope: %w", err)
	}
	fixture, err := buildSessionFixture(ctx, globalObjectCache, selections, policy)
	if err != nil {
		return nil, fmt.Errorf("build fixture: %w", err)
	}
	return fixture, nil
}

// writeSessionFixture serializes fixture to sessionTmpDir/fixture.json in the
// same {"operations": [...]} shape fixture.ts parses, so the
// spawned Node process reads the exact catalog validated by the Go middleware.
// sessionTmpDir already exists
// (buildMCPCommand creates it) and is cleaned up by terminateMCPSession, so
// this file needs no cleanup of its own.
func writeSessionFixture(sessionTmpDir string, fixture *Fixture) (string, error) {
	if fixture == nil {
		return "", fmt.Errorf("no fixture available for this session")
	}
	data, err := json.Marshal(struct {
		Operations []FixtureOperation `json:"operations"`
	}{Operations: fixture.Operations})
	if err != nil {
		return "", fmt.Errorf("marshal session fixture: %w", err)
	}
	path := filepath.Join(sessionTmpDir, "fixture.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", fmt.Errorf("write session fixture: %w", err)
	}
	return path, nil
}

func mcpSessionTmpDir(sessionID string) string {
	return filepath.Join(os.TempDir(), "fused-sandbox-"+sessionID)
}

// setupPipesAndStart initializes IO pipes and starts the Node process.
// A background goroutine calls cmd.Wait() to reap the zombie once it exits.
func setupPipesAndStart(cmd *exec.Cmd) (io.WriteCloser, io.ReadCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open stdin pipe")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open stdout pipe")
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start mcp server")
	}

	// Reap the process once it exits so it does not become a zombie.
	go func() { cmd.Wait() }()

	return stdin, stdout, nil
}

// registerMCPSession is shared by SSE and Streamable HTTP so expiry, idle
// termination, history events, and cleanup cannot drift by transport.
func registerMCPSession(ctx context.Context, sess *mcpSession) {
	sess.pendingRequests = make(map[string]struct{})
	touchMCPSession(sess)
	sess.idleTimer = time.AfterFunc(mcpSessionIdleTimeout(), func() { handleMCPSessionIdle(sess.sessionID) })
	mcpSessions.Lock()
	mcpSessions.m[sess.sessionID] = sess
	mcpSessions.Unlock()
	publishMCPSessionEvent(sess, "started", "")
	go terminateMCPSessionWhenContextEnds(ctx, sess.sessionID)
}

func mcpSessionIdleTimeout() time.Duration {
	return time.Duration(cfg.Sandbox.SessionMaxAgeSeconds) * time.Second
}

func handleMCPSessionIdle(sessionID string) {
	sess, ok := lookupMCPSession(sessionID)
	if !ok {
		return
	}
	sess.pendingMu.Lock()
	hasPending := len(sess.pendingRequests) > 0
	sess.pendingMu.Unlock()
	if hasPending {
		sess.idleTimer.Reset(mcpSessionIdleTimeout())
		return
	}
	terminateMCPSession(sessionID, "idle_timeout")
}

func terminateMCPSessionWhenContextEnds(ctx context.Context, sessionID string) {
	<-ctx.Done()
	reason := "engine_shutdown"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = "token_expired"
	} else if sess, ok := lookupMCPSession(sessionID); ok && sess.transport == "sse" {
		// An SSE session is deliberately tied to its request context, so ordinary
		// cancellation means the client closed that transport connection.
		reason = "client_disconnected"
	}
	terminateMCPSession(sessionID, reason)
}

// mcpSessionAuthContext accepts the same stable connected-user selectors a
// generated SDK sends, but only during the authenticated MCP handshake. The
// model-facing search_docs/execute schemas therefore contain no auth inputs.
func mcpSessionAuthContext(headers http.Header) (map[string]any, error) {
	endUserRef := strings.TrimSpace(headers.Get("X-Fused-End-User-Ref"))
	resourceID := strings.TrimSpace(headers.Get("X-Fused-Resource-ID"))
	if len(endUserRef) > 255 {
		return nil, errors.New("X-Fused-End-User-Ref is too long")
	}
	if resourceID != "" {
		if _, err := uuid.Parse(resourceID); err != nil {
			return nil, errors.New("X-Fused-Resource-ID must be a valid UUID")
		}
	}
	context := map[string]any{}
	if endUserRef != "" {
		context["fused_end_user_ref"] = endUserRef
	}
	if resourceID != "" {
		context["fused_resource_id"] = resourceID
	}
	return context, nil
}

// terminateMCPSession deletes only the transport session. The execution token
// remains independently revocable/expirable, while history keeps the reason.
func terminateMCPSession(sessionID, reason string) bool {
	mcpSessions.Lock()
	sess, ok := mcpSessions.m[sessionID]
	if ok {
		delete(mcpSessions.m, sessionID)
	}
	mcpSessions.Unlock()
	if !ok {
		return false
	}
	if sess.idleTimer != nil {
		sess.idleTimer.Stop()
	}
	if sess.cmd != nil && sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	os.RemoveAll(mcpSessionTmpDir(sessionID))
	if globalObjectCache != nil {
		globalObjectCache.DisconnectSDK(sess.appID)
	}
	publishMCPSessionEvent(sess, "ended", reason)
	return true
}

// publishMCPSessionEvent sends a session lifecycle event to NATS JetStream.
func publishMCPSessionEvent(sess *mcpSession, eventType, endReason string) {
	if globalNATSClient != nil && globalNATSClient.JS != nil {
		occurredAt := time.Now().UTC()
		eventData, _ := json.Marshal(map[string]any{
			"app_id": sess.appID, "app_token_id": sess.tokenID,
			"session_id": sess.sessionID, "protocol_version": sess.protocolVersion,
			"type": eventType, "end_reason": endReason, "timestamp": occurredAt,
			"last_activity_at": mcpSessionLastActivity(sess),
		})
		globalNATSClient.PublishJS(messaging.FusedEngineSessionSubject(sess.appID), eventData)
	}
}

func touchMCPSession(sess *mcpSession) {
	sess.activityMu.Lock()
	sess.lastActivityAt = time.Now().UTC()
	sess.activityMu.Unlock()
}

func mcpSessionLastActivity(sess *mcpSession) time.Time {
	sess.activityMu.Lock()
	defer sess.activityMu.Unlock()
	return sess.lastActivityAt
}

// setupSSEResponse configures the HTTP response for SSE streaming.
func setupSSEResponse(w http.ResponseWriter, sessionID string) (http.Flusher, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	fmt.Fprintf(w, "event: endpoint\ndata: /mcp/message?sessionId=%s\n\n", sessionID)
	flusher.Flush()
	return flusher, true
}

// processMCPStream forwards one session's child-process responses and keeps
// the SSE connection alive without sharing process state between sessions.
func processMCPStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, stdout io.ReadCloser, sessionID string) {
	stdoutLines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			stdoutLines <- scanner.Text()
		}
		close(stdoutLines)
	}()

	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case line, ok := <-stdoutLines:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", line)
			flusher.Flush()
			handleMCPResponse(line, sessionID)
		}
	}
}

func handleMCPResponse(line, sessionID string) {
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal([]byte(line), &msg) != nil {
		return
	}
	if len(msg.ID) == 0 {
		return
	}
	sess, ok := lookupMCPSession(sessionID)
	if !ok {
		return
	}
	completeMCPToolCall(sess, string(compactJSON(msg.ID)))
}

// mcpMessageHandler accepts JSON-RPC messages for an authenticated SSE session.
func mcpMessageHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId query param required")
		return
	}
	sess, ok := lookupMCPSession(sessionID)
	if !ok || sess.transport != "sse" {
		writeError(w, http.StatusNotFound, "mcp session not found or expired")
		return
	}
	if !messageUsesSessionToken(r, sess) {
		writeError(w, http.StatusUnauthorized, "invalid Authorization header")
		return
	}
	if !allowMessage(w, sess.appID) {
		return
	}
	body, err := readBoundedMCPMessageBody(w, r)
	if err != nil {
		return
	}
	callID := trackPendingRequest(body, sess)
	if callID != "" {
		go enforceToolCallTimeout(sess, callID)
	}
	resetMCPSessionIdleTimer(sess)
	if _, err = sess.stdin.Write(append(body, '\n')); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to send message to mcp server")
		return
	}
	touchMCPSession(sess)
	w.WriteHeader(http.StatusAccepted)
}

func messageUsesSessionToken(r *http.Request, sess *mcpSession) bool {
	token, ok := extractBearerAuthToken(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	// A session ID can appear in a URL or log, so the bearer still has to prove
	// ownership of the authenticated SSE session before dispatch is allowed.
	return subtle.ConstantTimeCompare([]byte(token), []byte(sess.token)) == 1
}

func readBoundedMCPMessageBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPMessageBodyBytes))
	if err == nil {
		return body, nil
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "mcp message body too large")
		return nil, err
	}
	writeError(w, http.StatusBadRequest, "failed to read body")
	return nil, err
}

// contextWithMCPIdempotencyIdentity generates a fresh idempotency key and
// request body hash for one MCP tool call, mirroring what the generated SDK
// client does automatically for every gRPC Execute call (ensureIdempotencyKey()
// + requestBodyHash() in base_client.ts). This hosted MCP proxy dispatches
// in-process instead of through that generated client, so without this, every
// MCP-originated call would carry no idempotency identity at all --
// disabling POST/PATCH retry safety (methodRequiresIdempotencyKeyForRetry in
// the engine package) and the idempotency cache (idempotency_cache.go) for
// the entire MCP surface.
//
// The key is fresh per incoming tools/call HTTP request, not per logical
// intent -- exactly like the generated SDK client's behavior: it's stable
// for Engine's own bounded retry loop within this one call, but a client
// resending the same tool call as a new HTTP request gets a new key, same as
// a caller invoking the generated SDK function a second time would.
func contextWithMCPIdempotencyIdentity(ctx context.Context, params map[string]any) context.Context {
	key := uuid.New().String()
	ctx = contextWithExecutionIdentity(ctx, key, hashRequestBody(params))
	return engine.ContextWithIdempotencyKeyPresent(ctx, true)
}

// hashRequestBody returns the SHA-256 hex digest of params' JSON encoding.
// encoding/json sorts map keys before marshaling, so this is deterministic
// for a given params map regardless of Go's map iteration order.
func hashRequestBody(params map[string]any) string {
	encoded, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// enforceToolCallTimeout terminates an SSE session whose child runtime never
// completes a tracked tool call, preventing pending work from defeating idle cleanup.
func enforceToolCallTimeout(sess *mcpSession, callID string) {
	timer := time.NewTimer(time.Duration(cfg.Sandbox.ToolCallTimeoutSeconds) * time.Second)
	defer timer.Stop()
	<-timer.C
	sess.pendingMu.Lock()
	_, stillPending := sess.pendingRequests[callID]
	if stillPending {
		delete(sess.pendingRequests, callID)
	}
	sess.pendingMu.Unlock()
	if stillPending {
		terminateMCPSession(sess.sessionID, "tool_call_timeout")
	}
}

// trackPendingRequest retains only an opaque request ID. Provider arguments
// are unnecessary for timeouts and must not become session state.
func trackPendingRequest(body []byte, sess *mcpSession) string {
	var request mcpJSONRPCRequest
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return trackMCPToolCall(request, sess)
}

func trackMCPToolCall(request mcpJSONRPCRequest, sess *mcpSession) string {
	if request.Method != "tools/call" || len(request.ID) == 0 {
		return ""
	}
	callID := string(compactJSON(request.ID))
	if callID == "null" {
		return ""
	}
	sess.pendingMu.Lock()
	sess.pendingRequests[callID] = struct{}{}
	sess.pendingMu.Unlock()
	return callID
}

func completeMCPToolCall(sess *mcpSession, callID string) {
	if callID == "" {
		return
	}
	sess.pendingMu.Lock()
	_, found := sess.pendingRequests[callID]
	if found {
		delete(sess.pendingRequests, callID)
	}
	sess.pendingMu.Unlock()
	if found {
		resetMCPSessionIdleTimer(sess)
		touchMCPSession(sess)
	}
}
