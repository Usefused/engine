package sandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
)

// TestMCPActiveTrafficKeepsSessionAndStreamAlive exercises both HTTP adapters beyond the former age limit.
func TestMCPActiveTrafficKeepsSessionAndStreamAlive(t *testing.T) {
	// Both adapters must share inactivity semantics even though SSE owns its session connection.
	for _, transport := range []string{"sse", mcpStreamableTransport} {
		t.Run(transport, func(t *testing.T) {
			// Virtual time covers twelve minutes of real handler traffic without slowing the suite.
			synctest.Test(t, func(t *testing.T) {
				sess := newMCPSessionIdleTestState(t, transport, nil)
				streamDone := startMCPSessionIdleTestStream(t, sess)
				// Requests arrive before each inactivity window but well beyond the old absolute deadline.
				for range 3 {
					time.Sleep(4 * time.Minute)
					sendMCPSessionIdleTestRequest(t, sess)
					synctest.Wait()
					assertMCPSessionIdleTestActive(t, sess)
					// Active traffic must preserve the stream, not merely the session map entry.
					select {
					case <-streamDone:
						t.Fatal("active SSE stream closed")
					default:
					}
				}
				time.Sleep(mcpSessionIdleTimeout())
				synctest.Wait()
				assertMCPSessionIdleTestEnded(t, sess)
				// Server-generated keepalives must not prevent abandoned session cleanup.
				select {
				case <-streamDone:
				default:
					t.Fatal("idle session left its SSE stream open")
				}
			})
		})
	}
}

// TestMCPPendingWorkDefersIdleCleanup proves a quiet long-running call gets a fresh idle window on completion.
func TestMCPPendingWorkDefersIdleCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newMCPSessionIdleTestState(t, "sse", nil)
		callID := trackPendingRequest(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute"}}`), sess)
		go enforceToolCallTimeout(sess, callID)
		callCtx, cancel := mcpSessionRequestContext(context.Background(), sess)
		defer cancel()
		time.Sleep(6 * time.Minute)
		synctest.Wait()
		assertMCPSessionIdleTestActive(t, sess)
		// Provider work shares cancellation, not the removed session-age deadline.
		if callCtx.Err() != nil {
			t.Fatalf("active bridge call canceled: %v", callCtx.Err())
		}
		completeMCPToolCall(sess, callID, `{"jsonrpc":"2.0","id":1,"result":{}}`, "")
		time.Sleep(mcpSessionIdleTimeout() - time.Second)
		synctest.Wait()
		assertMCPSessionIdleTestActive(t, sess)
		time.Sleep(time.Second)
		synctest.Wait()
		assertMCPSessionIdleTestEnded(t, sess)
	})
}

// TestMCPStaleIdleCallbackPreservesRecentActivity covers a timer already queued when traffic refreshes it.
func TestMCPStaleIdleCallbackPreservesRecentActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newMCPSessionIdleTestState(t, mcpStreamableTransport, nil)
		time.Sleep(mcpSessionIdleTimeout() - time.Second)
		touchMCPSession(sess)
		handleMCPSessionIdle(sess.sessionID)
		assertMCPSessionIdleTestActive(t, sess)
		time.Sleep(mcpSessionIdleTimeout())
		synctest.Wait()
		assertMCPSessionIdleTestEnded(t, sess)
		endedActivity := mcpSessionLastActivity(sess)
		touchMCPSession(sess)
		// Late completions cannot rewrite the durable end timestamp or rearm the retired timer.
		if !mcpSessionLastActivity(sess).Equal(endedActivity) {
			t.Fatal("late activity changed a retired session")
		}
	})
}

// TestMCPActiveSessionStillEndsAtTokenExpiry keeps recent traffic from extending authorization.
func TestMCPActiveSessionStillEndsAtTokenExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		expiry := time.Now().Add(6 * time.Minute)
		sess := newMCPSessionIdleTestState(t, mcpStreamableTransport, &expiry)
		time.Sleep(4 * time.Minute)
		sendMCPSessionIdleTestRequest(t, sess)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		assertMCPSessionIdleTestEnded(t, sess)
		// The persisted cause must remain credential expiry rather than inactivity.
		if got := canonicalMCPSessionEndReason(sess.lifecycleCtx, "runtime_failed"); got != "token_expired" {
			t.Fatalf("end reason = %q", got)
		}
	})
}

// TestMCPToolTimeoutStillEndsStuckSession ensures pending work cannot bypass its independent call budget.
func TestMCPToolTimeoutStillEndsStuckSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newMCPSessionIdleTestState(t, "sse", nil)
		callID := trackPendingRequest(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute"}}`), sess)
		go enforceToolCallTimeout(sess, callID)
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		failure := <-sess.sseFailures
		// A timed-out execute may already have mutated provider state, so the agent receives correlation and an explicit no-replay action before teardown.
		if !strings.Contains(failure.payload, `"id":1`) || !strings.Contains(failure.payload, mcpRuntimeOutcomeUnknownCode) || !strings.Contains(failure.payload, `"execute_request":"do_not_replay"`) {
			t.Fatalf("timeout failure = %s", failure.payload)
		}
		// The fallback grace lets a connected event stream drain the error before an unconsumed session is retired.
		time.Sleep(mcpSSEFailureDrainGrace)
		synctest.Wait()
		assertMCPSessionIdleTestEnded(t, sess)
	})
}

// newMCPSessionIdleTestState isolates external dependencies while keeping the production lifecycle and timers.
func newMCPSessionIdleTestState(t *testing.T, transport string, expiry *time.Time) *mcpSession {
	t.Helper()
	previousConfig, previousCache, previousNATS, previousValidator := cfg, globalObjectCache, globalNATSClient, globalTokenValidator
	localConfig := *cfg
	localConfig.Sandbox.SessionMaxAgeSeconds = 300
	localConfig.Sandbox.ToolCallTimeoutSeconds = 20 * 60
	cfg, globalObjectCache, globalNATSClient = &localConfig, nil, nil
	// Restore globals after session cleanup so no test reaches a real cache or event bus.
	t.Cleanup(func() {
		cfg, globalObjectCache, globalNATSClient, globalTokenValidator = previousConfig, previousCache, previousNATS, previousValidator
	})
	ctx, cancel := mcpSessionContext(context.Background(), expiry)
	sess := &mcpSession{
		appID: uuid.NewString(), sessionID: uuid.NewString(), tokenID: uuid.New(),
		transport: transport, protocolVersion: "2025-06-18", token: "test-token",
		stdin: &streamableWriteBuffer{}, responses: make(chan string, 1), cancel: cancel,
	}
	globalTokenValidator = &streamableTokenValidator{token: sess.token, tokenID: sess.tokenID}
	registerMCPSession(ctx, sess)
	// Every spawned lifecycle goroutine must finish before the virtual-time bubble exits.
	t.Cleanup(func() { terminateMCPSession(sess.sessionID, "client_terminated"); synctest.Wait() })
	return sess
}

// startMCPSessionIdleTestStream runs the real stream writer so context-only assertions cannot miss a disconnect.
func startMCPSessionIdleTestStream(t *testing.T, sess *mcpSession) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	recorder := httptest.NewRecorder()
	// Legacy SSE reads child stdout; Streamable HTTP authenticates a separate GET connection.
	if sess.transport == "sse" {
		reader, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })
		// The owned lifecycle must close the quiet pipe as well as the HTTP stream.
		go func() {
			processMCPStream(sess.lifecycleCtx, recorder, recorder, reader, sess.sessionID)
			close(done)
		}()
	} else {
		request := httptest.NewRequest(http.MethodGet, "/mcp/"+sess.appID, nil)
		request.Header.Set("Authorization", "Bearer "+sess.token)
		request.Header.Set(mcpSessionIDHeader, sess.sessionID)
		// The public GET path must remain attached to the same logical session as POST traffic.
		go func() { streamableTestRouter().ServeHTTP(recorder, request); close(done) }()
	}
	synctest.Wait()
	return done
}

// sendMCPSessionIdleTestRequest exercises accepted client traffic rather than directly resetting the timer.
func sendMCPSessionIdleTestRequest(t *testing.T, sess *mcpSession) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	// Each transport uses its real authenticated handler and native response status.
	if sess.transport == mcpStreamableTransport {
		sess.responses <- `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
		response := serveStreamableTestRequest(t, sess, sess.token, http.MethodPost, body)
		// A live map entry alone is insufficient: the existing session must still accept calls.
		if response.Code != http.StatusOK {
			t.Fatalf("active request status = %d", response.Code)
		}
		return
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId="+sess.sessionID, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+sess.token)
	response := httptest.NewRecorder()
	mcpMessageHandler(response, request)
	// SSE accepts client messages independently from its response stream.
	if response.Code != http.StatusAccepted {
		t.Fatalf("active SSE request status = %d", response.Code)
	}
}

// assertMCPSessionIdleTestActive checks both lookup visibility and runtime cancellation after timer work settles.
func assertMCPSessionIdleTestActive(t *testing.T, sess *mcpSession) {
	t.Helper()
	// Either losing registration or canceling the runtime breaks continued client use.
	if _, ok := lookupMCPSession(sess.sessionID); !ok || sess.lifecycleCtx.Err() != nil {
		t.Fatal("active session was terminated")
	}
}

// assertMCPSessionIdleTestEnded verifies cleanup reaches the runtime as well as the registry.
func assertMCPSessionIdleTestEnded(t *testing.T, sess *mcpSession) {
	t.Helper()
	// Both sides must finish so no child work survives an authorized termination.
	if _, ok := lookupMCPSession(sess.sessionID); ok || sess.lifecycleCtx.Err() == nil {
		t.Fatal("session cleanup did not finish")
	}
}
