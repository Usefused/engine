package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestMCPResponseScannerAllowsWrappedBoundedResult protects the gap between
// a bounded tool JSON value and its larger JSON-RPC text representation.
func TestMCPResponseScannerAllowsWrappedBoundedResult(t *testing.T) {
	inner, _ := json.Marshal(strings.Repeat(`"`, 300_000))
	line, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(inner)}}},
	})
	// This fixture must exceed the old scanner ceiling while remaining inside the admitted wire budget.
	if len(line) <= maxMCPPhysicalResultBytes || len(line) >= maxMCPResponseMessageBytes {
		t.Fatalf("wrapped fixture length = %d", len(line))
	}
	lines := make(chan string, 1)
	err := scanMCPResponseLines(context.Background(), io.NopCloser(strings.NewReader(string(line)+"\n")), lines)
	// An accepted result must survive framing without a synthetic runtime failure.
	if err != nil {
		t.Fatalf("scanMCPResponseLines() error = %v", err)
	}
	// The transport must preserve the full admitted envelope, not truncate escaped content.
	if got := <-lines; got != string(line) {
		t.Fatalf("scanner returned %d bytes, want %d", len(got), len(line))
	}
}

// TestMCPResponseScannerRejectsOversizedWireMessage keeps the scanner headroom
// finite even if a faulty child ignores its own tool output policy.
func TestMCPResponseScannerRejectsOversizedWireMessage(t *testing.T) {
	lines := make(chan string, 1)
	reader := io.NopCloser(strings.NewReader(strings.Repeat("x", maxMCPResponseMessageBytes+1) + "\n"))
	err := scanMCPResponseLines(context.Background(), reader, lines)
	// The scanner rejects the whole line instead of exposing a truncated response.
	if !errors.Is(err, bufio.ErrTooLong) || len(lines) != 0 {
		t.Fatalf("scanner error/queued lines = %v/%d", err, len(lines))
	}
}

// TestMCPResponseScannerCancellationUnblocksPendingSend proves expiry cannot
// strand the legacy SSE pump after its HTTP consumer has returned.
func TestMCPResponseScannerCancellationUnblocksPendingSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	// No receiver exists, forcing the completed child line to wait at the transport boundary.
	go func() { done <- scanMCPResponseLines(ctx, reader, make(chan string)) }()
	written := make(chan struct{})
	// Pipe completion proves the scanner consumed the line before cancellation is asserted.
	go func() {
		_, _ = io.WriteString(writer, "{}\n")
		close(written)
	}()
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("scanner did not consume the child line")
	}
	cancel()
	assertMCPScannerCanceled(t, done)
}

// TestMCPResponseScannerCancellationUnblocksRead protects sessions whose child
// is silent when session cancellation fires.
func TestMCPResponseScannerCancellationUnblocksRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	// An empty pipe makes the scanner wait inside Read rather than a channel send.
	go func() { done <- scanMCPResponseLines(ctx, reader, make(chan string)) }()
	cancel()
	assertMCPScannerCanceled(t, done)
}

// assertMCPScannerCanceled bounds the test wait so a leaked scanner is a
// deterministic failure rather than a hanging package test.
func assertMCPScannerCanceled(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		// Cancellation owns the pipe-close failure and must remain recognizable.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("scanner cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scanner remained blocked after session cancellation")
	}
}

// TestCanonicalMCPSessionEndReasonPrefersOwnedDeadline ensures child EOF and
// handler defers cannot race away the token-expiry audit outcome.
func TestCanonicalMCPSessionEndReasonPrefersOwnedDeadline(t *testing.T) {
	tokenCtx, cancelToken := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelToken()
	cases := []struct {
		name     string
		ctx      context.Context
		fallback string
		want     string
	}{
		{name: "child EOF at token expiry", ctx: tokenCtx, fallback: "runtime_failed", want: "token_expired"},
		{name: "handler return at token expiry", ctx: tokenCtx, fallback: "client_disconnected", want: "token_expired"},
		{name: "explicit termination before deadline", ctx: context.Background(), fallback: "client_terminated", want: "client_terminated"},
	}
	// Every competing cleanup path must preserve an already-fired owned deadline.
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// A stable lifecycle reason is required regardless of which cleanup goroutine wins.
			if got := canonicalMCPSessionEndReason(test.ctx, test.fallback); got != test.want {
				t.Fatalf("canonicalMCPSessionEndReason() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestMCPSessionRequestContextCancelsWithSession ensures an in-flight bridge
// request cannot keep a provider call alive after its runtime is terminated.
func TestMCPSessionRequestContextCancelsWithSession(t *testing.T) {
	lifecycleCtx, endSession := context.WithCancel(context.Background())
	defer endSession()
	ctx, cancel := mcpSessionRequestContext(context.Background(), &mcpSession{
		lifecycleCtx: lifecycleCtx,
	})
	defer cancel()
	endSession()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("bridge request remained alive after session cancellation")
	}
}

// TestMCPSessionRequestContextRejectsAlreadyEndedSession closes the small
// asynchronous AfterFunc window before provider dispatch begins.
func TestMCPSessionRequestContextRejectsAlreadyEndedSession(t *testing.T) {
	lifecycleCtx, endSession := context.WithCancel(context.Background())
	endSession()
	ctx, cancel := mcpSessionRequestContext(context.Background(), &mcpSession{
		lifecycleCtx: lifecycleCtx,
	})
	defer cancel()
	// A terminated session must be canceled synchronously, before callers can dispatch work.
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("bridge context error = %v", ctx.Err())
	}
}

// TestMCPToolTimeoutStopsWithSession ensures a long configured call timeout
// cannot retain an expired session and its catalogue until that timer fires.
func TestMCPToolTimeoutStopsWithSession(t *testing.T) {
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &mcpSession{lifecycleCtx: lifecycleCtx, pendingRequests: map[string]struct{}{"pending": {}}}
	done := make(chan struct{})
	// Completion is observed separately from pending-call state because teardown owns that state.
	go func() {
		enforceToolCallTimeout(sess, "pending")
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tool timeout goroutine outlived session cancellation")
	}
}
