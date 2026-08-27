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

	"github.com/Usefused/engine/internal/shared/models"
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
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
		Tools           []struct {
			Name        string                    `json:"name"`
			Description string                    `json:"description"`
			Meta        map[string]map[string]any `json:"_meta"`
			InputSchema struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
		Meta    map[string]mcpResultDelivery `json:"_meta"`
		IsError bool                         `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// mcpRuntimeNavigation captures the exact retained-result recovery request exposed to an agent.
type mcpRuntimeNavigation struct {
	RecoveryAction    string `json:"recovery_action"`
	ExecuteRequest    string `json:"execute_request"`
	ProviderExecution string `json:"provider_execution"`
	AutomaticReplay   bool   `json:"automatic_replay"`
	Session           struct {
		Scope               string `json:"scope"`
		SameSessionRequired bool   `json:"same_session_required"`
	} `json:"session"`
	NextRequest struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	} `json:"next_request"`
}

// mcpRuntimePaginationGuidance mirrors the bounded fields exposed to agents by search_docs.
type mcpRuntimePaginationGuidance struct {
	Supported            bool   `json:"supported"`
	CallerBoundSupported *bool  `json:"caller_bound_supported"`
	EngineMaxPages       *int   `json:"engine_max_pages"`
	ExactLookupRequired  bool   `json:"exact_lookup_required"`
	Usage                string `json:"usage"`
}

// mcpRuntimePaginationDocs mirrors the query result fields needed by the wire-level assertion.
type mcpRuntimePaginationDocs struct {
	Mode       string `json:"mode"`
	Operations []struct {
		OperationID string                       `json:"operation_id"`
		Pagination  mcpRuntimePaginationGuidance `json:"pagination"`
	} `json:"operations"`
}

// mcpRuntimeExecuteFailure mirrors the closed recovery fields visible in one execute tool error.
type mcpRuntimeExecuteFailure struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	RecoveryAction    string `json:"recovery_action"`
	ExecuteRequest    string `json:"execute_request"`
	ProviderExecution string `json:"provider_execution"`
	AutomaticReplay   bool   `json:"automatic_replay"`
}

// TestMCPBundledRuntimeAdvertisesSessionContract verifies the actual MCP SDK wire surfaces visible to hosts and agents.
func TestMCPBundledRuntimeAdvertisesSessionContract(t *testing.T) {
	client := startMCPLimitRuntime(t, "1")
	initialized := client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-session-contract-test", "version": "1.0.0"},
	})
	assertMCPRuntimeInitializeContract(t, initialized)
	assertMCPRuntimeExecuteContract(t, client.exchange(t, "tools/list", map[string]any{}))
}

// TestMCPBundledRuntimeSearchDocsPaginationGuidance verifies Go fixture policy reaches the public JSON-RPC result.
func TestMCPBundledRuntimeSearchDocsPaginationGuidance(t *testing.T) {
	fixture := &Fixture{Operations: []FixtureOperation{
		{OperationID: "users.list", ServiceID: "users", Name: "List users", Description: "List users", Method: "GET", Path: "/users", Responses: models.Responses{}, Pagination: FixturePagination{Supported: true, CallerBoundSupported: true, EngineMaxPages: 100}},
		{OperationID: "users.get", ServiceID: "users", Name: "Get user", Description: "Get one user", Method: "GET", Path: "/users/{id}", Responses: models.Responses{}, Pagination: FixturePagination{Supported: false}},
	}}
	client := startMCPLimitRuntimeWithFixture(t, "1", fixture)
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-pagination-docs-test", "version": "1.0.0"},
	})
	response := client.exchange(t, "tools/call", map[string]any{
		"name": "search_docs", "arguments": map[string]any{"query": "users", "limit": 2},
	})
	assertMCPRuntimePaginationDocs(t, response)
	exact := client.exchange(t, "tools/call", map[string]any{
		"name": "search_docs", "arguments": map[string]any{"operationId": "users.list"},
	})
	assertMCPRuntimeExactPaginationDocs(t, exact)
}

// TestMCPBundledRuntimePaginationErrorsPreserveCorrection verifies pre-provider shape and Engine intent errors across the shipped Node bundle.
func TestMCPBundledRuntimePaginationErrorsPreserveCorrection(t *testing.T) {
	var bridgeCalls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bridgeCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":            "mcp_pagination_not_supported",
			"error":           `mcp_pagination_not_supported: operation "gmail.users.messages.get" is not paginated; use call("gmail.users.messages.get", params) without a pagination option`,
			"recovery_action": "correct_execute_arguments", "execute_request": "correct_arguments", "provider_execution": "not_started", "automatic_replay": false,
		})
	}))
	defer bridge.Close()

	fixture := &Fixture{Operations: []FixtureOperation{{
		OperationID: "gmail.users.messages.get", ServiceID: "gmail", Name: "Get message", Method: "GET", Path: "/messages/{id}", Responses: models.Responses{}, Pagination: FixturePagination{Supported: false},
	}}}
	client := startMCPLimitRuntimeWithFixture(t, strings.TrimPrefix(bridge.URL, "http://127.0.0.1:"), fixture)
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-pagination-error-test", "version": "1.0.0"},
	})

	shapeFailure := decodeMCPRuntimeExecuteFailure(t, client.execute(t, `return await call("gmail.users.messages.get", {userId:"me",id:"message"}, {pagination:{maxPages:0}})`))
	assertMCPRuntimeArgumentCorrection(t, shapeFailure, "MCP_CALL_PAGINATION_INVALID")
	// Sandbox shape admission must stop before the Engine bridge receives a request.
	if bridgeCalls.Load() != 0 {
		t.Fatal("invalid physical pagination reached the Engine bridge")
	}

	intentFailure := decodeMCPRuntimeExecuteFailure(t, client.execute(t, `return await call("gmail.users.messages.get", {userId:"me",id:"message"}, {pagination:{maxPages:1}})`))
	assertMCPRuntimeArgumentCorrection(t, intentFailure, "mcp_pagination_not_supported")
	// The Engine-owned stable code must survive exactly one bridge request and outer execute formatting.
	if bridgeCalls.Load() != 1 || strings.Count(intentFailure.Message, "mcp_pagination_not_supported:") != 1 {
		t.Fatalf("typed pagination failure was duplicated or replayed: calls=%d message=%q", bridgeCalls.Load(), intentFailure.Message)
	}
}

// decodeMCPRuntimeExecuteFailure admits one real MCP tool error before checking its recovery semantics.
func decodeMCPRuntimeExecuteFailure(t *testing.T, response mcpRuntimeLimitResponse) mcpRuntimeExecuteFailure {
	t.Helper()
	var failure mcpRuntimeExecuteFailure
	// Execute failures must remain one bounded text item rather than a JSON-RPC transport error.
	if !response.Result.IsError || len(response.Result.Content) != 1 || json.Unmarshal([]byte(response.Result.Content[0].Text), &failure) != nil {
		t.Fatal("execute pagination failure did not return one structured tool error")
	}
	return failure
}

// assertMCPRuntimeArgumentCorrection checks the closed pre-provider action shared by syntax and Engine intent failures.
func assertMCPRuntimeArgumentCorrection(t *testing.T, failure mcpRuntimeExecuteFailure, wantCode string) {
	t.Helper()
	// Correctable pagination mistakes must never be labeled as unknown provider outcomes.
	if failure.Code != wantCode || failure.RecoveryAction != "correct_execute_arguments" || failure.ExecuteRequest != "correct_arguments" || failure.ProviderExecution != "not_started" || failure.AutomaticReplay {
		t.Fatalf("pagination correction = %+v, want code %q", failure, wantCode)
	}
}

// assertMCPRuntimePaginationDocs checks mixed GET guidance without depending on ranked order.
func assertMCPRuntimePaginationDocs(t *testing.T, response mcpRuntimeLimitResponse) {
	t.Helper()
	docs := decodeMCPRuntimePaginationDocs(t, response)
	byID := make(map[string]mcpRuntimePaginationGuidance, len(docs.Operations))
	// Identity indexing makes the assertion independent of search ranking ties.
	for _, operation := range docs.Operations {
		byID[operation.OperationID] = operation.Pagination
	}
	assertMCPRuntimePaginatedGuidance(t, byID["users.list"])
	assertMCPRuntimeUnpaginatedGuidance(t, byID["users.get"])
}

// decodeMCPRuntimePaginationDocs admits the real MCP envelope before semantic assertions.
func decodeMCPRuntimePaginationDocs(t *testing.T, response mcpRuntimeLimitResponse) mcpRuntimePaginationDocs {
	t.Helper()
	// Discovery must return one successful bounded text document through the real MCP SDK envelope.
	if response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatal("search_docs did not return one successful result")
	}
	var docs mcpRuntimePaginationDocs
	// Public pagination metadata must remain structured after fixture serialization and Node projection.
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &docs); err != nil || docs.Mode != "query" || len(docs.Operations) != 2 {
		t.Fatalf("invalid pagination documentation result: %v", err)
	}
	return docs
}

// assertMCPRuntimePaginatedGuidance verifies the reviewed automatic and caller-owned bounds independently.
func assertMCPRuntimePaginatedGuidance(t *testing.T, got mcpRuntimePaginationGuidance) {
	t.Helper()
	// Ranked guidance must withhold both the reusable boolean and numeric bound until exact lookup.
	if !got.Supported || !got.ExactLookupRequired || got.CallerBoundSupported != nil || got.EngineMaxPages != nil {
		t.Fatalf("ranked paginated list guidance = %+v", got)
	}
	assertMCPRuntimePaginatedUsage(t, got.Usage)
}

// assertMCPRuntimePaginatedUsage keeps exact-call recovery separate from ranked metadata admission.
func assertMCPRuntimePaginatedUsage(t *testing.T, usage string) {
	t.Helper()
	// The public instruction must permit only the exact two-argument call until exact lookup, without hinting at hidden bound syntax.
	if !strings.Contains(usage, `operationId "users.list"`) || !strings.Contains(usage, `call("users.list", params)`) || !strings.Contains(usage, "must not be reused") || strings.Contains(usage, "third") || strings.Contains(usage, "maxPages") || strings.Contains(usage, "engine_max_pages") {
		t.Fatalf("ranked paginated list usage = %q", usage)
	}
}

// assertMCPRuntimeExactPaginationDocs verifies numeric caller policy appears only after exact operation lookup.
func assertMCPRuntimeExactPaginationDocs(t *testing.T, response mcpRuntimeLimitResponse) {
	t.Helper()
	var result struct {
		Mode      string `json:"mode"`
		Operation struct {
			OperationID string                       `json:"operation_id"`
			Pagination  mcpRuntimePaginationGuidance `json:"pagination"`
		} `json:"operation"`
	}
	// Exact detail is the sole public surface allowed to expose a reusable Engine page bound.
	if response.Result.IsError || len(response.Result.Content) != 1 || json.Unmarshal([]byte(response.Result.Content[0].Text), &result) != nil || result.Mode != "operationId" {
		t.Fatal("exact pagination lookup did not return one operation detail")
	}
	assertMCPRuntimeExactPaginationValues(t, result.Operation.OperationID, result.Operation.Pagination)
}

// assertMCPRuntimeExactPaginationValues checks the full policy separately to keep envelope validation simple.
func assertMCPRuntimeExactPaginationValues(t *testing.T, operationID string, got mcpRuntimePaginationGuidance) {
	t.Helper()
	// Exact identity must accompany the full boolean and numeric bound.
	if operationID != "users.list" || !got.Supported || got.CallerBoundSupported == nil || !*got.CallerBoundSupported || got.EngineMaxPages == nil || *got.EngineMaxPages != 100 || got.ExactLookupRequired {
		t.Fatalf("exact pagination guidance = operation:%q pagination:%+v", operationID, got)
	}
}

// assertMCPRuntimeUnpaginatedGuidance verifies GET detail cannot advertise Engine traversal.
func assertMCPRuntimeUnpaginatedGuidance(t *testing.T, got mcpRuntimePaginationGuidance) {
	t.Helper()
	// Ranked unsupported guidance denies pagination in prose while withholding every exact-policy field uniformly.
	if got.Supported || got.CallerBoundSupported != nil || got.EngineMaxPages != nil || got.ExactLookupRequired {
		t.Fatalf("non-paginated get guidance = %+v", got)
	}
	// The public instruction must explain the one-request behavior to a fresh agent.
	if !strings.Contains(got.Usage, "does not traverse provider pages") {
		t.Fatalf("non-paginated get usage = %q", got.Usage)
	}
}

// assertMCPRuntimeInitializeContract checks the guidance available before any tool discovery.
func assertMCPRuntimeInitializeContract(t *testing.T, initialized mcpRuntimeLimitResponse) {
	t.Helper()
	// Initialize instructions are useful to capable hosts, while tools/list repeats the critical agent-facing rule.
	if initialized.Result.ProtocolVersion != "2025-03-26" || !strings.Contains(initialized.Result.Instructions, "already attached every execute call") || !strings.Contains(initialized.Result.Instructions, "Follow recovery_action and execute_request exactly") {
		t.Fatalf("initialize omitted session contract: %+v", initialized.Result)
	}
	// Fresh agents must defer to per-operation discovery instead of assuming every GET accepts a bound.
	if !strings.Contains(initialized.Result.Instructions, "exact operationId detail") || !strings.Contains(initialized.Result.Instructions, "Never reuse") {
		t.Fatal("initialize omitted operation-specific pagination guidance")
	}
}

// assertMCPRuntimeExecuteContract checks the script-session metadata on only the provider-capable tool.
func assertMCPRuntimeExecuteContract(t *testing.T, listed mcpRuntimeLimitResponse) {
	t.Helper()
	for _, tool := range listed.Result.Tools {
		// Only execute owns script session state; discovery remains free of irrelevant lifecycle metadata.
		if tool.Name != "execute" {
			continue
		}
		contract := tool.Meta["com.usefused/session"]
		scriptDescription := tool.InputSchema.Properties["script"].Description
		if !strings.Contains(tool.Description, "Never invent") || !strings.Contains(scriptDescription, "search_docs exact operationId detail") || !strings.Contains(scriptDescription, "caller_bound_supported=true") || contract["transport_session"] != "client_managed" || contract["session_id_input"] != false || contract["script_scope"] != "current_mcp_connection" || contract["automatic_execute_replay"] != false {
			t.Fatalf("execute session contract = description:%q metadata:%+v", tool.Description, contract)
		}
		return
	}
	t.Fatal("execute tool was not advertised")
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
	return startMCPLimitRuntimeWithDeadline(t, enginePort, 10*time.Second)
}

// startMCPLimitRuntimeWithDeadline allows deadline tests to outlive the production execute budget without changing it.
func startMCPLimitRuntimeWithDeadline(t *testing.T, enginePort string, timeout time.Duration) *mcpRuntimeLimitClient {
	return startMCPLimitRuntimeWithFixtureAndDeadline(t, enginePort, &Fixture{Operations: []FixtureOperation{}}, timeout)
}

// startMCPLimitRuntimeWithFixture starts the bundled runtime with an explicit public catalogue.
func startMCPLimitRuntimeWithFixture(t *testing.T, enginePort string, fixture *Fixture) *mcpRuntimeLimitClient {
	t.Helper()
	return startMCPLimitRuntimeWithFixtureAndDeadline(t, enginePort, fixture, 10*time.Second)
}

// startMCPLimitRuntimeWithFixtureAndDeadline owns process setup for both empty and documentation fixtures.
func startMCPLimitRuntimeWithFixtureAndDeadline(t *testing.T, enginePort string, fixture *Fixture, timeout time.Duration) *mcpRuntimeLimitClient {
	t.Helper()
	// Go-only environments cannot execute Node integration tests, while release builds always include Node.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for bundled MCP runtime integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	sessionID := uuid.NewString()
	// The production helper creates this exact per-session temporary directory; cleanup owns only that directory.
	t.Cleanup(func() { _ = os.RemoveAll(mcpSessionTmpDir(sessionID)) })
	cmd, err := buildMCPCommand(ctx, sessionID, fixture)
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

// TestMCPBundledRuntimeRetainedRetrieval proves physical controls, retention, and session paging share one real HTTP dispatch.
func TestMCPBundledRuntimeRetainedRetrieval(t *testing.T) {
	var calls atomic.Int32
	var paginationForwarded atomic.Bool
	bridge := startMCPRuntimeRetentionBridge(t, &calls, &paginationForwarded)
	port := strings.TrimPrefix(bridge.URL, "http://127.0.0.1:")
	client := startMCPLimitRuntime(t, port)
	client.exchange(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fused-page-test", "version": "1.0.0"},
	})
	stored := client.execute(t, `return await call("fixture.read", {}, {pagination:{maxPages:1}})`)
	reference := retainedMCPResultReference(t, stored)
	assertMCPRuntimeStoredAdmission(t, stored, calls.Load(), paginationForwarded.Load())
	navigation := decodeMCPRuntimeNavigation(t, stored)
	continued := client.exchange(t, "tools/call", map[string]any{"name": navigation.NextRequest.Tool, "arguments": navigation.NextRequest.Arguments})
	assertMCPRuntimeContinuation(t, continued, calls.Load())
	retrieved := client.execute(t, `const result=session.get("`+reference+`"); return {items:result.items.slice(0,1), length:result.body.length}`)
	assertMCPRuntimeRetainedRead(t, retrieved, calls.Load())
	invalidGet := client.execute(t, `return session.get("`+reference+`", {path:"/transactions"})`)
	assertMCPRuntimeInvalidGet(t, invalidGet, calls.Load())
	assertMCPRuntimeTransactionPages(t, client, reference)
	// All continuation requests must read the original snapshot, not dispatch the operation again.
	if calls.Load() != 1 {
		t.Fatal("paging repeated the bridge call")
	}
}

// startMCPRuntimeRetentionBridge serves one large synthetic provider result through the real child bridge.
func startMCPRuntimeRetentionBridge(t *testing.T, calls *atomic.Int32, paginationForwarded *atomic.Bool) *httptest.Server {
	t.Helper()
	rows := make([]map[string]any, 120)
	// Distinct row positions prove that retained paging neither drops nor duplicates provider results.
	for index := range rows {
		rows[index] = map[string]any{"id": index, "amount": index, "raw": strings.Repeat("PRIVATE_SENTINEL", 40)}
	}
	// The synthetic bridge exercises real runtime HTTP without accessing any connected account.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveMCPRuntimeRetentionBridge(w, r, calls, paginationForwarded, rows)
	}))
	t.Cleanup(bridge.Close)
	return bridge
}

// serveMCPRuntimeRetentionBridge validates physical pagination separation before returning retained test data.
func serveMCPRuntimeRetentionBridge(w http.ResponseWriter, r *http.Request, calls *atomic.Int32, paginationForwarded *atomic.Bool, rows []map[string]any) {
	calls.Add(1)
	var request struct {
		Params     map[string]any `json:"params"`
		Pagination struct {
			MaxPages int `json:"maxPages"`
		} `json:"pagination"`
	}
	// The real child must keep Engine pagination intent outside provider parameters.
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Pagination.MaxPages != 1 {
		http.Error(w, "invalid synthetic pagination envelope", http.StatusBadRequest)
		return
	}
	// Provider parameters cannot acquire an Engine-owned field through serialization.
	if _, leaked := request.Params["pagination"]; leaked {
		http.Error(w, "pagination leaked into provider params", http.StatusBadRequest)
		return
	}
	paginationForwarded.Store(true)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{
		"items": []string{"fixture-one", "fixture-two"}, "body": strings.Repeat("PRIVATE_SENTINEL", 9000),
		"transactions": rows,
	}})
}

// assertMCPRuntimeStoredAdmission checks that retention follows one paginated provider dispatch without preview leakage.
func assertMCPRuntimeStoredAdmission(t *testing.T, stored mcpRuntimeLimitResponse, calls int32, paginationForwarded bool) {
	t.Helper()
	// Retention begins only after the bridge accepts the separate canonical pagination control.
	if !paginationForwarded {
		t.Fatal("bundled runtime did not forward the physical pagination intent")
	}
	// Automatic previews must not expose synthetic private scalar sentinels.
	if calls != 1 || strings.Contains(stored.Result.Content[0].Text, "PRIVATE_SENTINEL") {
		t.Fatal("automatic preview exposed scalar content")
	}
}

// decodeMCPRuntimeNavigation validates the compact action contract and returns its exact continuation request.
func decodeMCPRuntimeNavigation(t *testing.T, stored mcpRuntimeLimitResponse) mcpRuntimeNavigation {
	t.Helper()
	var navigation mcpRuntimeNavigation
	// The stored envelope itself supplies a directly executable session-only continuation.
	if json.Unmarshal([]byte(stored.Result.Content[0].Text), &navigation) != nil || navigation.RecoveryAction != "continue_stored_result" || navigation.ExecuteRequest != "use_next_request" || navigation.ProviderExecution != "complete" || navigation.AutomaticReplay || navigation.Session.Scope != "current_mcp_connection" || !navigation.Session.SameSessionRequired || navigation.NextRequest.Tool != "execute" {
		t.Fatalf("stored navigation omitted session recovery: %s", stored.Result.Content[0].Text)
	}
	return navigation
}

// assertMCPRuntimeContinuation proves an advertised stored read cannot repeat provider execution.
func assertMCPRuntimeContinuation(t *testing.T, continued mcpRuntimeLimitResponse, calls int32) {
	t.Helper()
	// Executing the advertised continuation must read retained state without another bridge request.
	if continued.Result.IsError || calls != 1 {
		t.Fatal("advertised retained continuation failed or repeated the provider call")
	}
}

// assertMCPRuntimeRetainedRead verifies the selected value and its bounded delivery audit metadata.
func assertMCPRuntimeRetainedRead(t *testing.T, retrieved mcpRuntimeLimitResponse, calls int32) {
	t.Helper()
	// Exact selected fields are returned through the existing tool, with no second bridge invocation.
	if retrieved.Result.IsError || retrieved.Result.Content[0].Text != `{"items":["fixture-one"],"length":144000}` || calls != 1 {
		t.Fatal("retained retrieval failed or repeated the bridge call")
	}
	metadata := retrieved.Result.Meta["com.usefused/execute"]
	// Runtime-owned metadata must survive the actual MCP SDK envelope serializer.
	if metadata.Delivery != "inline" || metadata.RetainedReads != 1 || metadata.UnavailableReads != 0 {
		t.Fatal("retained read audit metadata was missing or invalid")
	}
}

// assertMCPRuntimeInvalidGet proves invalid session access fails before retention or provider work is attributed.
func assertMCPRuntimeInvalidGet(t *testing.T, invalidGet mcpRuntimeLimitResponse, calls int32) {
	t.Helper()
	invalidMetadata := invalidGet.Result.Meta["com.usefused/execute"]
	// Extra arguments fail before a retained read or another provider dispatch can be attributed.
	if !invalidGet.Result.IsError || !strings.Contains(invalidGet.Result.Content[0].Text, "MCP_SESSION_GET_ARGUMENTS_INVALID") || invalidMetadata.RetainedReads != 0 || calls != 1 {
		t.Fatal("session.get silently accepted an option or changed retained/provider read counts")
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
