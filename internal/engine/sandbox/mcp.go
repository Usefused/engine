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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/runtime"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxMCPMessageBodyBytes = 256 * 1024

// mcpSseHandler handles SSE connections for MCP sessions.
//
// Isolation guarantee: each connection spawns a brand-new `node` process.
// No memory, globals, or file handles are shared between sessions.
func mcpSseHandler(w http.ResponseWriter, r *http.Request) {
	artifactIDHex, token, ok := extractMCPParams(w, r)
	if !ok {
		return
	}
	authContext, err := mcpSessionAuthContext(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// ── Rate limit: SSE connections per SDK-ID ─────────────────────────────
	if !allowSSEConnect(w, artifactIDHex) {
		return
	}

	if _, err := validateTokenWithCache(r.Context(), artifactIDHex, token); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	// S4.3 Handshake: Load the SDK scope and Fused Objects into memory refcounted cache
	// Inject the user's token into the context so RegistryClient can authenticate as the user
	ctx := context.WithValue(r.Context(), "sdk-token", token)
	if err := globalObjectCache.ConnectSDK(ctx, artifactIDHex); err != nil {
		writeError(w, http.StatusUnauthorized, fmt.Sprintf("Failed to handshake SDK cache: %v", err))
		return
	}
	defer globalObjectCache.DisconnectSDK(artifactIDHex)

	sessionID := uuid.NewString()

	// Each session gets a context. It will be cancelled if the client disconnects,
	// or if the idle timer fires (see registerMCPSession).
	ctx, cancel := context.WithCancel(r.Context())

	// Sessions get an operation catalog scoped to exactly what this artifact's
	// owner selected; catalog construction must succeed before the child starts.
	// Built from a Registry round trip, so a failure here is treated the same
	// as the ConnectSDK handshake failure above -- the session never starts
	// rather than serving a session whose search_docs/call() catalog can't be
	// trusted to match its actual scope.
	fixture, err := prepareSessionFixture(ctx, artifactIDHex)
	if err != nil {
		cancel()
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to build mcp fixture: %v", err))
		return
	}

	cmd, err := buildMCPCommand(ctx, sessionID, fixture)
	if err != nil {
		cancel()
		writeError(w, http.StatusInternalServerError, "failed to prepare mcp runtime")
		return
	}

	stdin, stdout, err := setupPipesAndStart(cmd)
	if err != nil {
		cancel()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	injectedResp := make(chan string, 100)
	registerMCPSession(sessionID, artifactIDHex, token, cmd, stdin, cancel, injectedResp, fixture, authContext)
	publishMCPSessionEvent(artifactIDHex, sessionID, "started")

	defer cleanupMCPSession(sessionID, artifactIDHex, cmd, cancel)

	flusher, ok := setupSSEResponse(w, sessionID)
	if !ok {
		return
	}

	processMCPStream(ctx, w, flusher, stdout, artifactIDHex, sessionID, injectedResp)
}

// extractMCPParams extracts and validates required URL parameters and headers.
func extractMCPParams(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	artifactIDHex := chi.URLParam(r, "id")
	if artifactIDHex == "" {
		writeError(w, http.StatusBadRequest, "sdk id required")
		return "", "", false
	}

	token, ok := extractBearerAuthToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "Authorization header required")
		return "", "", false
	}
	return artifactIDHex, token, true
}

func validateTokenWithCache(ctx context.Context, artifactIDHex, token string) (uuid.UUID, error) {
	if globalTokenValidator == nil {
		return uuid.Nil, auth.ErrUnauthorized
	}
	cacheKey := sha256CacheKey(artifactIDHex, token)
	now := time.Now()

	tokenCache.RLock()
	entry, found := tokenCache.m[cacheKey]
	tokenCache.RUnlock()
	if found && now.Before(entry.expiry) && entry.accountID != uuid.Nil {
		return entry.accountID, nil
	}

	artifactID, err := uuid.Parse(artifactIDHex)
	if err != nil {
		return uuid.Nil, err
	}
	accountID, err := globalTokenValidator.Validate(ctx, artifactID, token)
	if err != nil {
		return uuid.Nil, err
	}

	tokenCache.Lock()
	tokenCache.m[cacheKey] = tokenCacheEntry{accountID: accountID, expiry: now.Add(time.Minute)}
	tokenCache.Unlock()
	return accountID, nil
}

func sha256CacheKey(artifactIDHex, token string) string {
	sum := sha256.Sum256([]byte(artifactIDHex + "\x00" + token))
	return hex.EncodeToString(sum[:])
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
//     that is cleaned up by cleanupMCPSession.
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

// prepareSessionFixture loads the SDK's scope and derives its own MCP
// fixture from it (mcp_session_fixture.go) -- split out of mcpSseHandler so
// that handler's own error-handling stays a single branch instead of two.
func prepareSessionFixture(ctx context.Context, artifactIDHex string) (*Fixture, error) {
	selections, err := validateAndParseScope(ctx, globalObjectCache, artifactIDHex)
	if err != nil {
		return nil, fmt.Errorf("load sdk scope: %w", err)
	}
	fixture, err := buildSessionFixture(ctx, globalObjectCache, artifactIDHex, selections)
	if err != nil {
		return nil, fmt.Errorf("build fixture: %w", err)
	}
	return fixture, nil
}

// writeSessionFixture serializes fixture to sessionTmpDir/fixture.json in the
// same {"operations": [...]} shape LoadFixture/fixture.ts parse, so the
// spawned Node process reads the exact catalog validated by the Go middleware.
// sessionTmpDir already exists
// (buildMCPCommand creates it) and is cleaned up by cleanupMCPSession, so
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

// registerMCPSession saves the session state safely in the global map and starts the idle timer.
func registerMCPSession(sessionID, artifactIDHex, token string, cmd *exec.Cmd, stdin io.WriteCloser, cancel context.CancelFunc, injectedResp chan string, fixture *Fixture, authContext map[string]any) {
	sessionIdleTimeout := time.Duration(cfg.Sandbox.SessionMaxAgeSeconds) * time.Second

	sess := &mcpSession{
		artifactID:      artifactIDHex,
		sessionID:       sessionID,
		cmd:             cmd,
		stdin:           stdin,
		cancel:          cancel,
		pendingRequests: make(map[string]pendingReq),
		injectedResp:    injectedResp,
		token:           token,
		fixture:         fixture,
		authContext:     authContext,
	}

	// The idle timer kills the session if no activity occurs within the timeout.
	sess.idleTimer = time.AfterFunc(sessionIdleTimeout, func() {
		mcpSessions.RLock()
		s, ok := mcpSessions.m[sessionID]
		mcpSessions.RUnlock()
		if !ok {
			return
		}

		s.pendingMu.Lock()
		hasPending := len(s.pendingRequests) > 0
		s.pendingMu.Unlock()

		if !hasPending {
			// Idle timeout reached with no pending requests — terminate session.
			if s.cmd.Process != nil {
				s.cmd.Process.Kill()
			}
			s.cancel()
		} else {
			// Something is in flight, give it more time. The toolCallTimeout will
			// catch it if it hangs.
			s.idleTimer.Reset(sessionIdleTimeout)
		}
	})

	mcpSessions.Lock()
	mcpSessions.m[sessionID] = sess
	mcpSessions.Unlock()
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

// cleanupMCPSession kills the Node process, removes the session from the
// registry, cleans up the per-session temp dir, and publishes an end event.
func cleanupMCPSession(sessionID, artifactIDHex string, cmd *exec.Cmd, cancel context.CancelFunc) {
	// Kill the process explicitly before cancelling the context so there is no
	// race between context cancellation and process termination.
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
	cancel()

	mcpSessions.Lock()
	if sess, ok := mcpSessions.m[sessionID]; ok && sess.idleTimer != nil {
		sess.idleTimer.Stop()
	}
	delete(mcpSessions.m, sessionID)
	mcpSessions.Unlock()

	// Clean up the per-session temp directory.
	os.RemoveAll(mcpSessionTmpDir(sessionID))

	publishMCPSessionEvent(artifactIDHex, sessionID, "ended")
}

// publishMCPSessionEvent sends a session lifecycle event to NATS JetStream.
func publishMCPSessionEvent(artifactIDHex, sessionID, eventType string) {
	if globalNATSClient != nil && globalNATSClient.JS != nil {
		eventData, _ := json.Marshal(map[string]any{
			"artifact_id": artifactIDHex,
			"session_id":  sessionID,
			"type":        eventType,
			"timestamp":   time.Now(),
		})
		globalNATSClient.PublishJS(messaging.FusedEngineSessionSubject(artifactIDHex), eventData)
	}
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

// processMCPStream reads stdout from the Node process and forwards to the client.
func processMCPStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, stdout io.ReadCloser, artifactIDHex, sessionID string, injectedResp chan string) {
	stdoutLines := make(chan string)

	go func() {
		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 1*1024*1024)
		scanner.Buffer(buf, 1*1024*1024) // 1 MB per line
		for scanner.Scan() {
			stdoutLines <- scanner.Text()
		}
		close(stdoutLines)
	}()

	// Send a keep-alive ping every 15 seconds to prevent proxies from dropping the connection
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	// multiplex node stdout and native Go dispatcher responses.
	for {
		select {
		case <-ctx.Done():
			// Client disconnected or session was forcefully canceled
			return

		case <-pingTicker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()

		case line, ok := <-stdoutLines:
			if !ok {
				return
			}
			if strings.HasPrefix(line, "___FUSED_SPAN___:") {
				handleFusedSpan(line, artifactIDHex, sessionID)
				continue
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", line)
			flusher.Flush()
			handleAnalyticsResponse(line, sessionID, artifactIDHex)

		case line, ok := <-injectedResp:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", line)
			flusher.Flush()
			handleAnalyticsResponse(line, sessionID, artifactIDHex)
		}
	}
}

// handleFusedSpan processes and forwards a telemetry span to NATS.
func handleFusedSpan(line, artifactIDHex, sessionID string) {
	if globalNATSClient != nil && globalNATSClient.JS != nil {
		spanJSON := strings.TrimPrefix(line, "___FUSED_SPAN___:")
		var spanData map[string]any

		if err := json.Unmarshal([]byte(spanJSON), &spanData); err == nil {
			spanData["artifact_id"] = artifactIDHex
			spanData["session_id"] = sessionID
			spanData["timestamp"] = time.Now()
			eventData, _ := json.Marshal(spanData)
			globalNATSClient.PublishJS(messaging.FusedEngineAnalyticsSubject(artifactIDHex), eventData)
		}
	}
}

// handleAnalyticsResponse parses tool call responses for latency and error tracking.
func handleAnalyticsResponse(line, sessionID, artifactIDHex string) {
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return
	}

	idVal, ok := msg["id"]
	if !ok {
		return
	}
	idStr := fmt.Sprintf("%v", idVal)

	mcpSessions.RLock()
	sess, ok := mcpSessions.m[sessionID]
	mcpSessions.RUnlock()

	if ok {
		sess.pendingMu.Lock()
		req, found := sess.pendingRequests[idStr]
		if found {
			delete(sess.pendingRequests, idStr)
		}
		sess.pendingMu.Unlock()

		if found {
			publishAnalyticsForRequest(msg, req, artifactIDHex, sessionID)

			// Reset idle timer when a tool call completes.
			sessionIdleTimeout := time.Duration(cfg.Sandbox.SessionMaxAgeSeconds) * time.Second
			sess.idleTimer.Reset(sessionIdleTimeout)
		}
	}
}

// publishAnalyticsForRequest checks for errors in the MCP response and publishes
// analytics. Params (sanitised) and result content are included because the user
// owns the MCP executor — this data is safe to persist for debugging.
func publishAnalyticsForRequest(msg map[string]any, req pendingReq, artifactIDHex, sessionID string) {
	latencyMs := time.Since(req.startTime).Milliseconds()
	failed := false

	// Determine failure from the MCP JSON-RPC response shape.
	var resultContent any
	if _, hasErr := msg["error"]; hasErr {
		failed = true
	} else if res, hasRes := msg["result"].(map[string]any); hasRes {
		if isErr, _ := res["isError"].(bool); isErr {
			failed = true
		}
		// Capture result content for observability (no credentials in result).
		resultContent = res["content"]
	}

	if globalNATSClient != nil && globalNATSClient.JS != nil {
		paramsJSON, _ := json.Marshal(sanitiseParams(req.arguments))
		resultJSON, _ := json.Marshal(resultContent)
		eventData, _ := json.Marshal(map[string]any{
			"artifact_id":   artifactIDHex,
			"session_id":    sessionID,
			"endpoint_name": req.endpointName,
			"latency_ms":    latencyMs,
			"failed":        failed,
			"timestamp":     time.Now(),
			"params":        json.RawMessage(paramsJSON),
			"result":        json.RawMessage(resultJSON),
		})
		globalNATSClient.PublishJS(messaging.FusedEngineAnalyticsSubject(artifactIDHex), eventData)
	}
}

// mcpMessageHandler handles incoming messages sent to the MCP server.
func mcpMessageHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId query param required")
		return
	}

	mcpSessions.RLock()
	sess, ok := mcpSessions.m[sessionID]
	mcpSessions.RUnlock()

	if !ok {
		writeError(w, http.StatusNotFound, "mcp session not found or expired")
		return
	}
	if !messageUsesSessionToken(r, sess) {
		writeError(w, http.StatusUnauthorized, "invalid Authorization header")
		return
	}

	// ── Rate limit: tools/call messages per SDK-ID ─────────────────────────
	if !allowMessage(w, sess.artifactID) {
		return
	}

	body, err := readBoundedMCPMessageBody(w, r)
	if err != nil {
		return
	}

	callID, endpointName, _ := trackPendingRequest(body, sess)

	// Enforce the per-tool-call timeout. If the Node process does not respond
	// within the configured window, the entire session is killed.
	if callID != "" {
		go enforceToolCallTimeout(sess, callID, endpointName)
	}

	// Reset idle timer when a new message is received.
	sessionIdleTimeout := time.Duration(cfg.Sandbox.SessionMaxAgeSeconds) * time.Second
	sess.idleTimer.Reset(sessionIdleTimeout)

	_, err = sess.stdin.Write(append(body, '\n'))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to send message to mcp server")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func messageUsesSessionToken(r *http.Request, sess *mcpSession) bool {
	token, ok := extractBearerAuthToken(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	// Why compare to the session token instead of trusting sessionId alone:
	// /mcp/message carries only a public-ish session id in the URL, so the
	// bearer must still prove it owns the authenticated SSE session.
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

// enforceToolCallTimeout waits for the configured tool-call deadline. If the
// pending request is still in-flight when the timer fires, the session is
// killed and the client's SSE stream will close.
func enforceToolCallTimeout(sess *mcpSession, callID, endpointName string) {
	timeout := time.Duration(cfg.Sandbox.ToolCallTimeoutSeconds) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	<-timer.C

	sess.pendingMu.Lock()
	_, stillPending := sess.pendingRequests[callID]
	if stillPending {
		delete(sess.pendingRequests, callID)
	}
	sess.pendingMu.Unlock()

	if stillPending {
		// The tool call did not complete in time — kill the session.
		if sess.cmd != nil && sess.cmd.Process != nil {
			sess.cmd.Process.Kill()
		}
		if sess.cancel != nil {
			sess.cancel()
		}
	}
}

// trackPendingRequest parses the JSON-RPC request and records it for latency
// tracking and timeout enforcement. Returns the call ID and tool name (empty
// strings if the message is not a tools/call).
func trackPendingRequest(body []byte, sess *mcpSession) (callID, endpointName string, params map[string]any) {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", "", nil
	}

	method, _ := msg["method"].(string)
	if method != "tools/call" {
		return "", "", nil
	}

	idVal, ok := msg["id"]
	if !ok {
		return "", "", nil
	}
	idStr := fmt.Sprintf("%v", idVal)

	p, ok := msg["params"].(map[string]any)
	if !ok {
		return "", "", nil
	}
	name, _ := p["name"].(string)

	arguments, _ := p["arguments"].(map[string]any)

	sess.pendingMu.Lock()
	sess.pendingRequests[idStr] = pendingReq{
		endpointName: name,
		startTime:    time.Now(),
		arguments:    arguments,
	}
	sess.pendingMu.Unlock()

	return idStr, name, arguments
}
