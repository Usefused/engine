package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestMCPModernMetadataNeedsNoNodeOrStartupBudget proves metadata cannot allocate even a transient child.
func TestMCPModernMetadataNeedsNoNodeOrStartupBudget(t *testing.T) {
	fixture := installMCPModernFixture(t)
	oldStarts, oldMessages := sessionStartRateLimiter, messageRateLimiter
	sessionStartRateLimiter, messageRateLimiter = newRateLimitStore(0, 0), newRateLimitStore(0, 40)
	t.Cleanup(func() { sessionStartRateLimiter, messageRateLimiter = oldStarts, oldMessages })
	t.Setenv("PATH", t.TempDir())
	active := activeMCPSessionCount()
	for index, arguments := range []map[string]any{{}, {"query": "payment"}, {"operationId": "absent"}, {"query": 12}, {"limit": 0}, {"section": "invalid"}} {
		response := httptest.NewRecorder()
		fixture.router.ServeHTTP(response, newMCPModernRequest(t, fixture, index, "tools/list", nil))
		decodeMCPModernResult(t, response)
		response = httptest.NewRecorder()
		fixture.router.ServeHTTP(response, newMCPModernRequest(t, fixture, index+10, "tools/call", map[string]any{"name": "search_docs", "arguments": arguments}))
		result := decodeMCPModernResult(t, response)
		// Invalid argument types must become tool errors without attempting process startup or panicking in telemetry.
		if index >= 3 && result["isError"] != true {
			t.Fatalf("invalid arguments accepted: %v", arguments)
		}
	}
	// An empty limiter map proves no start was attempted, even when a transient child could otherwise escape session counts.
	if len(sessionStartRateLimiter.entries) != 0 || activeMCPSessionCount() != active {
		t.Fatal("metadata attempted sandbox startup")
	}
	denied := newMCPModernRequest(t, fixture, 30, "tools/list", nil)
	denied.Header.Set("Authorization", "Bearer denied")
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, denied)
	// Direct metadata must keep the same credential admission as execution.
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized metadata status = %d", response.Code)
	}
	messageRateLimiter = newRateLimitStore(0, 0)
	response = httptest.NewRecorder()
	fixture.router.ServeHTTP(response, newMCPModernRequest(t, fixture, 31, "tools/list", nil))
	// Removing process startup does not remove per-request rate limiting.
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("request budget was bypassed: %d", response.Code)
	}
}

// TestMCPMetadataMatchesNode checks the shipped pure search bundle against its native JavaScript implementation.
func TestMCPMetadataMatchesNode(t *testing.T) {
	// Only this cross-interpreter parity test requires Node; direct HTTP metadata tests deliberately remove it.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node required for metadata parity")
	}
	data, err := os.ReadFile("../../../runtime/mcp/fixture.json")
	// The legacy spike fixture predates required server metadata; this test owns a complete copy.
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	// Preserve the spike's canonical operation contracts while supplying only test-owned server identity.
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["server"] = map[string]any{"name": "metadata-test", "title": "Metadata test", "version": "1.0.0", "description": "Read synthetic operation documentation."}
	data, err = json.Marshal(document)
	// Encoding failure is a fixture error, not a reason to weaken production admission.
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	// Production fixture loading must validate the same bytes passed to both interpreters.
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := LoadFixture(path)
	// The existing fixture exercises path params, request schemas, and response documentation.
	if err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs("../../../runtime/mcp/dist/metadata.js")
	// Resolve the built source explicitly so Node never imports another checkout's bundle.
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{{}, {"query": "GitHub repositories"}, {"query": "répertoire 日本語"}, {"operationId": "github.listRepos"}, {"operationId": "github.listRepos", "section": "params_schema", "schemaPath": "/properties/username"}, {"operationId": "missing"}, {"query": false}} {
		actual, err := runMCPMetadata(context.Background(), fixture, args)
		// Every admitted search mode must work in-process, including unknown IDs and input errors.
		if err != nil {
			t.Fatalf("search %v: %v", args, err)
		}
		expected := nodeMCPMetadata(t, module, fixture, args)
		// Semantic equality covers schema packing, lexical ranking, Unicode, pointers, and output envelopes.
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("search %v mismatch\nactual: %#v\nexpected: %#v", args, actual, expected)
		}
	}
	actual, err := runMCPMetadata(context.Background(), nil, nil)
	// Tool schemas and descriptions must come from exactly the same shared declarations as Node.
	if err != nil || !reflect.DeepEqual(actual, nodeMCPMetadata(t, module, nil, nil)) {
		t.Fatalf("tool declaration parity failed: %v", err)
	}
}

// nodeMCPMetadata evaluates the same authorized JSON in Node as an independent interpreter oracle.
func nodeMCPMetadata(t *testing.T, module string, fixture *Fixture, arguments map[string]any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"catalogue": fixture, "arguments": arguments})
	// Test input must cross JSON just as production catalogue input does.
	if err != nil {
		t.Fatal(err)
	}
	script := `import {readFileSync} from 'node:fs'; import {pathToFileURL} from 'node:url'; const m=await import(pathToFileURL(process.argv[1])); const p=JSON.parse(readFileSync(0,'utf8')); console.log(JSON.stringify(p.catalogue ? m.search(p.catalogue,p.arguments) : m.listTools()));`
	command := exec.Command("node", "--input-type=module", "-e", script, module)
	command.Stdin = strings.NewReader(string(payload))
	output, err := command.CombinedOutput()
	// A failed oracle must not be interpreted as a successful empty catalogue.
	if err != nil {
		t.Fatalf("node metadata: %v: %s", err, output)
	}
	var result map[string]any
	// Decode both implementations through JSON so integer and map representation differences cannot mask semantic failures.
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestMCPMetadataCancellation rejects cancelled work without creating a background evaluator.
func TestMCPMetadataCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runMCPMetadata(ctx, nil, nil)
	// No output is valid once the caller has cancelled ownership of the request.
	if err == nil {
		t.Fatal("cancelled metadata request succeeded")
	}
}

// TestMCPMetadataBundleHasNoExecutionDependencies guards the no-I/O metadata build boundary.
func TestMCPMetadataBundleHasNoExecutionDependencies(t *testing.T) {
	data, err := os.ReadFile("../../../runtime/mcp/dist/metadata-bundle.js")
	// Missing generated metadata is a release build failure, never a fallback to Node.
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"node:fs", "node:vm", "node:http", "runExecute", "SessionState", "callClientOptionsFromEnv"} {
		// Trusted metadata must not accidentally bundle provider execution or filesystem dependencies.
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("metadata bundle contains %q", forbidden)
		}
	}
}
