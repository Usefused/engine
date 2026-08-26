package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mcpRuntimeLimitClient drives the real bundled child using the same bounded line transport as Engine.
type mcpRuntimeLimitClient struct {
	stdin   io.WriteCloser
	scanner *bufio.Scanner
}

// mcpRuntimeLimitResponse exposes only fields needed to verify the public tool-result contract.
type mcpRuntimeLimitResponse struct {
	Error  json.RawMessage `json:"error"`
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// TestMCPBundledRuntimeOutputLimits proves actual JSON-RPC envelopes preserve usable sessions at both boundaries.
func TestMCPBundledRuntimeOutputLimits(t *testing.T) {
	client := startMCPLimitRuntime(t)
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-limit-test", "version": "1.0.0"},
	})
	accepted := client.execute(t, `return '"'.repeat(300000)`)
	// Escaping the admitted text makes its outer envelope larger than the old one-MiB scanner ceiling.
	if accepted.Result.IsError || len(accepted.Result.Content) != 1 {
		t.Fatalf("escaped output was not admitted: isError=%v, content=%d", accepted.Result.IsError, len(accepted.Result.Content))
	}
	var value string
	// The full result must survive transport; a partial string is not an acceptable fallback.
	if err := json.Unmarshal([]byte(accepted.Result.Content[0].Text), &value); err != nil || value != strings.Repeat(`"`, 300000) {
		t.Fatal("escaped output was corrupted or truncated")
	}
	for _, script := range []string{`return "secret".repeat(200000)`, `throw new Error("secret".repeat(200000))`} {
		assertMCPRuntimeLimitResult(t, client.execute(t, script))
	}
	recovered := client.execute(t, `return {ok: true}`)
	// A size rejection must fail one invocation, not kill the reusable MCP session.
	if recovered.Result.IsError || len(recovered.Result.Content) != 1 || recovered.Result.Content[0].Text != `{"ok":true}` {
		t.Fatal("runtime was unusable after output rejection")
	}
}

// startMCPLimitRuntime reuses production fixture and process setup without requiring a live Engine or provider.
func startMCPLimitRuntime(t *testing.T) *mcpRuntimeLimitClient {
	t.Helper()
	// Go-only environments cannot execute Node integration tests, while release builds always include Node.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for bundled MCP runtime integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sessionID := uuid.NewString()
	// The production helper creates this exact per-session temporary directory; cleanup owns only that directory.
	t.Cleanup(func() { _ = os.RemoveAll(mcpSessionTmpDir(sessionID)) })
	cmd, err := buildMCPCommand(ctx, sessionID, &Fixture{Operations: []FixtureOperation{}})
	// Fixture admission and bundle staging must succeed before transport behavior can be asserted.
	if err != nil {
		t.Fatal(err)
	}
	// These scripts never dispatch provider calls; a syntactically valid loopback port satisfies runtime setup.
	cmd.Env = append(cmd.Env, "FUSED_ENGINE_PORT=1")
	stdin, stdout, err := setupPipesAndStart(cmd)
	// Process failures are surfaced directly instead of being confused with output-limit behavior.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxMCPResponseMessageBytes)
	return &mcpRuntimeLimitClient{stdin: stdin, scanner: scanner}
}

// execute sends a normal tools/call envelope, keeping serialization assertions at the public boundary.
func (client *mcpRuntimeLimitClient) execute(t *testing.T, script string) mcpRuntimeLimitResponse {
	t.Helper()
	return client.exchange(t, "tools/call", map[string]any{"name": "execute", "arguments": map[string]string{"script": script}})
}

// exchange performs one sequential JSON-RPC round trip without retaining provider or credential data.
func (client *mcpRuntimeLimitClient) exchange(t *testing.T, method string, params map[string]any) mcpRuntimeLimitResponse {
	t.Helper()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	// Test messages must be valid before exercising the production decoder.
	if err != nil {
		t.Fatal(err)
	}
	// A broken child pipe must fail this test instead of waiting for an unrelated timeout.
	if _, err := client.stdin.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	// The bounded scanner catches both premature child exit and oversized protocol envelopes.
	if !client.scanner.Scan() {
		t.Fatalf("runtime returned no response: %v", client.scanner.Err())
	}
	var response mcpRuntimeLimitResponse
	// Parse the actual public envelope to detect serialization or protocol regressions.
	if err := json.Unmarshal(client.scanner.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	// Application size failures belong in tool results, never in protocol-error fallbacks.
	if len(response.Error) != 0 {
		t.Fatalf("unexpected JSON-RPC error: %s", response.Error)
	}
	return response
}

// assertMCPRuntimeLimitResult requires a stable bounded failure with no rejected data fragments.
func assertMCPRuntimeLimitResult(t *testing.T, response mcpRuntimeLimitResponse) {
	t.Helper()
	// One small text error is the canonical output-limit response shape.
	if !response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatal("oversized output did not produce one tool error")
	}
	text := response.Result.Content[0].Text
	var failure struct {
		Code string `json:"code"`
	}
	// Both normal results and thrown messages share the same terminal result policy.
	if err := json.Unmarshal([]byte(text), &failure); err != nil || failure.Code != "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED" {
		t.Fatalf("output failure code = %q", failure.Code)
	}
	// Rejected values must neither be echoed nor retained as a partial model-visible result.
	if len(text) > 1024 || strings.Contains(text, "secret") {
		t.Fatal("output rejection exposed rejected content or exceeded its bounded error shape")
	}
}
