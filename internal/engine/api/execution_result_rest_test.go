package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExecutionReadHandleDigest requires one fixed-size caller-held credential.
func TestExecutionReadHandleDigest(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("X-Execution-Read-Handle", strings.Repeat("a", 64))
	first, ok := executionReadHandleDigest(request)
	if !ok || len(first) != 64 || first == strings.Repeat("a", 64) {
		t.Fatalf("expected hashed read handle, got %q valid=%v", first, ok)
	}
	request.Header.Add("X-Execution-Read-Handle", strings.Repeat("b", 64))
	if _, ok := executionReadHandleDigest(request); ok {
		t.Fatal("duplicate read handles must be rejected")
	}
}

// TestSearchablePathsUseImmutableAppDescriptor rejects a search policy outside the exact app bundle.
func TestSearchablePathsUseImmutableAppDescriptor(t *testing.T) {
	manifest := json.RawMessage(`{"schemaVersion":1,"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"searchable":["customer.id"],"selectedOperations":[{"service":"crm","operation":"readCustomer","serviceId":"11111111-1111-4111-8111-111111111111","serviceVersionId":"22222222-2222-4222-8222-222222222222","endpointId":"33333333-3333-4333-8333-333333333333"}]}`)
	paths, err := searchablePathsForExecutionApp(manifest)
	if err != nil || len(paths) != 1 || paths[0] != "customer.id" {
		t.Fatalf("expected persisted allowlist, got %v, %v", paths, err)
	}
	// A legacy named surface must not supply the app-level search allowlist.
	legacy := json.RawMessage(`{"schemaVersion":1,"capabilities":[{"name":"onboard","searchable":["customer.id"]}]}`)
	if _, err := searchablePathsForExecutionApp(legacy); err == nil {
		t.Fatal("legacy capability manifest must fail closed")
	}
}

// TestExecutionSearchRequestRejectsCapabilityQuery keeps searches scoped to the app version.
func TestExecutionSearchRequestRejectsCapabilityQuery(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/apps/app/executions?capability=onboard", nil)
	if _, _, err := executionSearchRequest(request); err == nil {
		t.Fatal("secondary capability selector must be rejected")
	}
	request = httptest.NewRequest("GET", "/v1/apps/app/executions", nil)
	if _, limit, err := executionSearchRequest(request); err != nil || limit != 20 {
		t.Fatalf("expected default app search, got limit=%d error=%v", limit, err)
	}
}

// TestExecutionSearchWhereRejectsDuplicateKeys keeps filter admission unambiguous.
func TestExecutionSearchWhereRejectsDuplicateKeys(t *testing.T) {
	if _, err := executionSearchWhere([]string{`{"status":"running","status":"succeeded"}`}); err == nil {
		t.Fatal("duplicate search keys must be rejected")
	}
	where, err := executionSearchWhere([]string{`{"status":"succeeded"}`})
	if err != nil || string(where["status"]) != `"succeeded"` {
		t.Fatalf("expected canonical valid search, got %v, %v", where, err)
	}
}
