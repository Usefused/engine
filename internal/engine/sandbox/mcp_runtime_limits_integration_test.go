package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
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
		Meta    map[string]mcpResultDelivery `json:"_meta"`
		IsError bool                         `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// TestMCPBundledRuntimeOutputLimits proves large admitted JSON survives retention while visible output stays small.
func TestMCPBundledRuntimeOutputLimits(t *testing.T) {
	client := startMCPLimitRuntime(t, "1")
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-limit-test", "version": "1.0.0"},
	})
	accepted := client.execute(t, `return '"'.repeat(300000)`)
	reference := retainedMCPResultReference(t, accepted)
	selected := client.execute(t, `return {length:session.get("`+reference+`").length, intact:session.get("`+reference+`") === '"'.repeat(300000)}`)
	// Validate the whole retained string in the runtime without returning it to the client.
	if selected.Result.IsError || selected.Result.Content[0].Text != `{"length":300000,"intact":true}` {
		t.Fatal("escaped snapshot was corrupted or truncated")
	}
	assertMCPRuntimeLimitResult(t, client.execute(t, `return "secret".repeat(200000)`), "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED")
	assertMCPRuntimeLimitResult(t, client.execute(t, `throw new Error("secret".repeat(200000))`), "MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED")
	recovered := client.execute(t, `return {ok: true}`)
	// A size rejection must fail one invocation, not kill the reusable MCP session.
	if recovered.Result.IsError || len(recovered.Result.Content) != 1 || recovered.Result.Content[0].Text != `{"ok":true}` {
		t.Fatal("runtime was unusable after output rejection")
	}
}

// startMCPLimitRuntime reuses production process setup with an explicit synthetic bridge port.
func startMCPLimitRuntime(t *testing.T, enginePort string) *mcpRuntimeLimitClient {
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
	// Tests own the loopback bridge; production providers and credentials are never involved.
	cmd.Env = append(cmd.Env, "FUSED_ENGINE_PORT="+enginePort)
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

// assertMCPRuntimeLimitResult checks each boundary's stable code without exposing rejected content.
func assertMCPRuntimeLimitResult(t *testing.T, response mcpRuntimeLimitResponse, code string) {
	t.Helper()
	// One small text error is the canonical output-limit response shape.
	if !response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatal("oversized output did not produce one tool error")
	}
	text := response.Result.Content[0].Text
	var failure struct {
		Code string `json:"code"`
	}
	// Stored-result admission and non-navigable errors have distinct model-facing policies.
	if err := json.Unmarshal([]byte(text), &failure); err != nil || failure.Code != code {
		t.Fatalf("output failure code = %q", failure.Code)
	}
	// Rejected values must neither be echoed nor retained as a partial model-visible result.
	if len(text) > 1024 || strings.Contains(text, "secret") {
		t.Fatal("output rejection exposed rejected content or exceeded its bounded error shape")
	}
}

// retainedMCPResultReference validates only navigation metadata, never the private retained value.
func retainedMCPResultReference(t *testing.T, response mcpRuntimeLimitResponse) string {
	t.Helper()
	// An admitted overflow is a successful bounded tool result, not a protocol or execution error.
	if response.Result.IsError || len(response.Result.Content) != 1 || len(response.Result.Content[0].Text) > 64<<10 {
		t.Fatal("expected one bounded stored-result envelope")
	}
	var envelope struct {
		Code      string `json:"code"`
		Complete  bool   `json:"complete"`
		Reference string `json:"result_ref"`
	}
	// Never confuse a partial preview with a complete operation result.
	if json.Unmarshal([]byte(response.Result.Content[0].Text), &envelope) != nil || envelope.Code != "MCP_RESULT_STORED" || envelope.Complete || !strings.HasPrefix(envelope.Reference, "fused-result:") {
		t.Fatal("invalid stored-result navigation contract")
	}
	return envelope.Reference
}

// TestMCPBundledRuntimeRetainedRetrieval proves field discovery and multi-page retrieval share one real HTTP dispatch.
func TestMCPBundledRuntimeRetainedRetrieval(t *testing.T) {
	var calls atomic.Int32
	rows := make([]map[string]any, 120)
	for index := range rows {
		rows[index] = map[string]any{"id": index, "amount": index, "raw": strings.Repeat("PRIVATE_SENTINEL", 40)}
	}
	// The synthetic bridge exercises real runtime HTTP without accessing any connected account.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{
			"items": []string{"fixture-one", "fixture-two"}, "body": strings.Repeat("PRIVATE_SENTINEL", 9000),
			"transactions": rows,
		}})
	}))
	t.Cleanup(bridge.Close)
	port := strings.TrimPrefix(bridge.URL, "http://127.0.0.1:")
	client := startMCPLimitRuntime(t, port)
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-page-test", "version": "1.0.0"},
	})
	stored := client.execute(t, `return await call("fixture.read", {})`)
	reference := retainedMCPResultReference(t, stored)
	// Automatic previews must not expose synthetic private scalar sentinels.
	if strings.Contains(stored.Result.Content[0].Text, "PRIVATE_SENTINEL") {
		t.Fatal("automatic preview exposed scalar content")
	}
	retrieved := client.execute(t, `const result=session.get("`+reference+`"); return {items:result.items.slice(0,1), length:result.body.length}`)
	// Exact selected fields are returned through the existing tool, with no second bridge invocation.
	if retrieved.Result.IsError || retrieved.Result.Content[0].Text != `{"items":["fixture-one"],"length":144000}` || calls.Load() != 1 {
		t.Fatal("retained retrieval failed or repeated the bridge call")
	}
	metadata := retrieved.Result.Meta["com.usefused/execute"]
	// Runtime-owned metadata must survive the actual MCP SDK envelope serializer.
	if metadata.Delivery != "inline" || metadata.RetainedReads != 1 || metadata.UnavailableReads != 0 {
		t.Fatal("retained read audit metadata was missing or invalid")
	}
	assertMCPRuntimeTransactionPages(t, client, reference)
	// All continuation requests must read the original snapshot, not dispatch the operation again.
	if calls.Load() != 1 {
		t.Fatal("paging repeated the bridge call")
	}
}

// assertMCPRuntimeTransactionPages checks ordered whole-row coverage through the existing public execute tool.
func assertMCPRuntimeTransactionPages(t *testing.T, client *mcpRuntimeLimitClient, reference string) {
	t.Helper()
	offset := 0
	for pageCount := 0; pageCount < 120; pageCount++ {
		script := fmt.Sprintf(`return session.page(%q, {path:"/transactions",fields:["id","amount"],offset:%d})`, reference, offset)
		response := client.exchange(t, "tools/call", map[string]any{"name": "execute", "arguments": map[string]any{"script": script, "outputBudgetBytes": 1024}})
		page := decodeMCPRuntimePage(t, response)
		assertMCPPageRows(t, page, offset)
		offset += page.Returned
		// Only complete coverage terminates the continuation chain.
		if page.Complete {
			// A terminal cursor is valid only after every expected synthetic row has appeared once.
			if offset != 120 || page.NextOffset != nil {
				t.Fatal("page claimed completion before full coverage")
			}
			return
		}
		// Nonterminal pages must supply the exact next offset, not a guessed provider cursor.
		if page.NextOffset == nil || *page.NextOffset != offset {
			t.Fatal("page continuation was missing or incorrect")
		}
	}
	t.Fatal("page traversal did not terminate")
}

// assertMCPPageRows separates synthetic data integrity from transport and continuation assertions.
func assertMCPPageRows(t *testing.T, page mcpRuntimePage, offset int) {
	t.Helper()
	// Each successful page advances the exact stored position; no dropped or invented rows are acceptable.
	if page.Offset != offset || page.Total != 120 || page.Returned != len(page.Items) || page.Returned == 0 {
		t.Fatal("page range was inconsistent or did not advance")
	}
	for index, item := range page.Items {
		// Projection must preserve data and ordering, and exclude the large unselected field.
		if len(item) != 2 || item["id"] != offset+index || item["amount"] != offset+index {
			t.Fatal("page projection changed or omitted a row")
		}
	}
}

// mcpRuntimePage exposes only synthetic projected rows and exact continuation metadata for integration assertions.
type mcpRuntimePage struct {
	Offset     int              `json:"offset"`
	Total      int              `json:"total"`
	Returned   int              `json:"returned"`
	NextOffset *int             `json:"nextOffset"`
	Complete   bool             `json:"complete"`
	Items      []map[string]int `json:"items"`
}

// decodeMCPRuntimePage verifies the real MCP SDK preserves the per-call byte budget and trusted audit metadata.
func decodeMCPRuntimePage(t *testing.T, response mcpRuntimeLimitResponse) mcpRuntimePage {
	t.Helper()
	// Pages must remain inline; re-retaining a page would turn a bounded read into another discovery round trip.
	if response.Result.IsError || len(response.Result.Content) != 1 || len(response.Result.Content[0].Text) > 1024 {
		t.Fatal("page exceeded its negotiated budget or failed")
	}
	var page mcpRuntimePage
	// A stored-result envelope cannot masquerade as a page with projected row coverage.
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &page); err != nil {
		t.Fatal("page was not valid projected JSON")
	}
	metadata := response.Result.Meta["com.usefused/execute"]
	// Navigation audit contains counters and policy only, with no second provider receipt.
	if metadata.Delivery != "inline" || metadata.RetainedReads != 1 || metadata.OutputBudgetBytes != 1024 {
		t.Fatal("page audit metadata was missing or invalid")
	}
	return page
}
