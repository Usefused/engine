package sandbox

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMCPBundledRuntimeCancellation exercises the shipped bundle's real delay, deadline, fetch abort, and session recovery.
func TestMCPBundledRuntimeCancellation(t *testing.T) {
	var calls atomic.Int32
	disconnected := make(chan struct{}, 1)
	bridge := httptest.NewServer(mcpCancellationBridge(&calls, disconnected))
	t.Cleanup(bridge.Close)
	client := startMCPLimitRuntimeWithDeadline(t, strings.TrimPrefix(bridge.URL, "http://127.0.0.1:"), 55*time.Second)
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-cancellation-test", "version": "1.0.0"},
	})
	result := client.execute(t, `await call("fixture.first"); await new Promise(resolve => setTimeout(resolve, 15000)); const second=await call("fixture.second"); setTimeout(() => call("fixture.detached"), 100); return {text:decodeBase64(second.text),binary:atob(encodeBase64("ok")),buffer:typeof Buffer};`)
	// The user's delayed-sequence shape still succeeds; only the fixture responses reach assertions.
	if result.Result.IsError || result.Result.Content[0].Text != `{"text":"ok","binary":"ok","buffer":"undefined"}` {
		t.Fatal("bundled runtime failed the supported delay/decoding contract")
	}
	client.execute(t, `await sleep(200); return true;`)
	// A completed invocation cannot leak its detached timer into the next tool request.
	if calls.Load() != 2 {
		t.Fatal("a completed invocation dispatched a detached operation")
	}
	timedOut := client.execute(t, `try { await call("fixture.hang"); } catch {} await call("fixture.afterTimeout");`)
	assertMCPRuntimeLimitResult(t, timedOut, "MCP_EXECUTE_TIMEOUT")
	// A script catching an abort cannot hide termination from the host-owned telemetry contract.
	if timedOut.Result.Meta["com.usefused/execute"].ExecutionOutcome != "timed_out" {
		t.Fatal("timeout outcome was missing from trusted runtime metadata")
	}
	// A real disconnected bridge request is stronger evidence than a rejected wrapper promise.
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("timed-out runtime did not abort the bridge HTTP request")
	}
	recovered := client.execute(t, `await sleep(25); return "ready";`)
	// One timed-out invocation must not poison session state or permit a fourth provider dispatch.
	if recovered.Result.IsError || recovered.Result.Content[0].Text != `"ready"` || calls.Load() != 3 {
		t.Fatal("runtime failed recovery or dispatched after timeout")
	}
}

// mcpCancellationBridge serves synthetic responses before holding the third call for actual HTTP cancellation.
func mcpCancellationBridge(calls *atomic.Int32, disconnected chan<- struct{}) http.HandlerFunc {
	// The bridge intentionally has no access to Engine credentials or production providers.
	return func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// A hanging body proves the shared AbortSignal covers reads, not just connection setup.
		if count == 3 {
			_, _ = w.Write([]byte(`{"result":`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			disconnected <- struct{}{}
			return
		}
		_, _ = w.Write([]byte(`{"result":{"text":"b2s="}}`))
	}
}
