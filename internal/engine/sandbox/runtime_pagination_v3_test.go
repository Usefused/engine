package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/google/uuid"
)

// TestRuntimePaginationV3ValidatesEffectiveRequestTypeAtSnapshotBoundary rejects policies whose effective target type mismatches the endpoint.
func TestRuntimePaginationV3ValidatesEffectiveRequestTypeAtSnapshotBoundary(t *testing.T) {
	initial := ""
	policy := &paginationpolicy.Config{
		Version: paginationpolicy.Version,
		Request: []paginationpolicy.RequestStep{{
			State: "cursor", Target: paginationpolicy.RequestTarget{Location: "query", Name: "cursor"},
			ValueType: "string", Initial: &paginationpolicy.Scalar{Type: "string", String: &initial}, Apply: "all",
		}},
		Response: paginationpolicy.ResponsePlan{
			Items:  paginationpolicy.ItemsSource{Path: "$.items"},
			Values: []paginationpolicy.ResponseValue{{Name: "next", Source: paginationpolicy.ValueSource{Location: "body", Path: "$.next", ValueType: "string"}}},
		},
		Continuation: []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}},
		Termination:  paginationpolicy.Termination{StopOnMissingValues: []string{"next"}, RepeatedValue: "error"},
		Limits:       paginationpolicy.Limits{MaxPages: 10, MaxItems: 100, MaxBytes: 1 << 20, MaxDurationMs: 5_000},
	}
	metadata := &fusedobject.ServiceMetadata{Pagination: policy}
	endpoint := fusedobject.Endpoint{Parameters: fusedobject.Parameters{{Name: "cursor", In: "query", Type: "integer"}}}

	err := validateRuntimePagination(metadata, []fusedobject.Endpoint{endpoint})
	if err == nil || !strings.Contains(err.Error(), "request_target_invalid") {
		t.Fatalf("snapshot target validation error = %v", err)
	}

	endpoint.Parameters[0].Type = "string"
	if err := validateRuntimePagination(metadata, []fusedobject.Endpoint{endpoint}); err != nil {
		t.Fatalf("valid effective service pagination was rejected: %v", err)
	}
}

// TestRuntimePaginationV3RegistrySnapshotExecutesProviderNeutralPolicy proves one canonical Registry contract survives decode, mapping, and dispatch unchanged.
func TestRuntimePaginationV3RegistrySnapshotExecutesProviderNeutralPolicy(t *testing.T) {
	calls := &atomic.Int32{}
	provider := httptest.NewServer(runtimeHeaderCursorProvider(t, calls))
	t.Cleanup(provider.Close)
	serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	requested := []store.WorkspaceServiceVersion{{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}}
	payload := runtimeHeaderCursorContractJSON(serviceID, versionID, endpointID, provider.URL)

	snapshots, err := decodeRuntimeContractsResponse(strings.NewReader(payload), requested)
	// Registry decode must accept the complete current envelope before any provider request begins.
	if err != nil {
		t.Fatalf("decode runtime contract: %v", err)
	}
	// A single requested version must remain a single typed snapshot before any execution mapping occurs.
	if len(snapshots) != 1 || len(snapshots[0].Endpoints) != 1 {
		t.Fatalf("runtime snapshots = %#v", snapshots)
	}
	expectedPolicy := runtimeHeaderCursorPolicy()
	// Exact typed equality catches loss or rewriting of any v3 policy field during GraphQL decoding.
	if !reflect.DeepEqual(snapshots[0].Endpoints[0].Pagination, expectedPolicy) {
		t.Fatalf("decoded pagination = %#v, want %#v", snapshots[0].Endpoints[0].Pagination, expectedPolicy)
	}
	object := fusedToIntegrationObject(&snapshots[0].ServiceMetadata, snapshots[0].Endpoints[0])
	// Dispatch mapping must preserve the canonical endpoint policy rather than deriving source-specific behavior.
	if !reflect.DeepEqual((*paginationpolicy.Config)(object.Pagination), expectedPolicy) {
		t.Fatalf("mapped pagination = %#v, want %#v", object.Pagination, expectedPolicy)
	}

	collector := newBoundedJSONResponseCollector()
	status, err := engine.NewDispatcher().ExecuteStream(context.Background(), fusedToService(&snapshots[0].ServiceMetadata), object, nil, nil, nil, collector)
	// Successful pagination is one logical provider response even though two pages were requested.
	if err != nil || status != http.StatusOK {
		t.Fatalf("dispatch status = %d, error = %v", status, err)
	}
	result, err := collector.Result()
	// The collector validates that the dispatcher emitted exactly one JSON response contract.
	if err != nil {
		t.Fatalf("collect aggregate: %v", err)
	}
	assertRuntimeHeaderCursorAggregate(t, int(calls.Load()), result.Body)
}

// runtimeHeaderCursorProvider returns two deterministic pages and requires the continuation header on the second request.
func runtimeHeaderCursorProvider(t *testing.T, calls *atomic.Int32) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expectedHeaders := []string{"", "cursor-2"}
		call := int(calls.Load())
		// A third request would prove the missing-header termination contract was lost.
		if call >= len(expectedHeaders) {
			t.Errorf("unexpected provider request %d", call+1)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		// Header equality proves continuation state crossed the canonical request target unchanged.
		if got := request.Header.Get("X-Page-Cursor"); got != expectedHeaders[call] {
			t.Errorf("request %d cursor = %q, want %q", call+1, got, expectedHeaders[call])
		}
		writer.Header().Set("Content-Type", "application/json")
		// Only the first page advertises continuation, making the second page the natural terminal page.
		if call == 0 {
			writer.Header().Set("X-Next-Cursor", "cursor-2")
		}
		bodies := []string{`{"items":[1],"source":"canonical"}`, `{"items":[2],"source":"ignored-second-page"}`}
		_, _ = writer.Write([]byte(bodies[call]))
		calls.Add(1)
	})
}

// runtimeHeaderCursorPolicy returns the source-neutral v3 policy shared by formal Registry adapter parity coverage.
func runtimeHeaderCursorPolicy() *paginationpolicy.Config {
	return &paginationpolicy.Config{
		Version: paginationpolicy.Version,
		Request: []paginationpolicy.RequestStep{{State: "cursor", Target: paginationpolicy.RequestTarget{
			Location: paginationpolicy.RequestHeader, Name: "X-Page-Cursor",
		}, ValueType: paginationpolicy.ValueString, Apply: paginationpolicy.ApplyAll}},
		Response: paginationpolicy.ResponsePlan{
			Items: paginationpolicy.ItemsSource{Path: "$.items"},
			Values: []paginationpolicy.ResponseValue{{Name: "next_cursor", Source: paginationpolicy.ValueSource{
				Location: paginationpolicy.SourceHeader, Name: "X-Next-Cursor", ValueType: paginationpolicy.ValueString,
			}}},
		},
		Continuation: []paginationpolicy.ContinuationStep{{Kind: paginationpolicy.ContinuationToken, State: "cursor", ResponseValue: "next_cursor"}},
		Termination:  paginationpolicy.Termination{StopOnMissingValues: []string{"next_cursor"}, RepeatedValue: paginationpolicy.RepeatedError},
		Limits:       paginationpolicy.Limits{MaxPages: 100, MaxItems: 10_000, MaxBytes: 16_777_216, MaxDurationMs: 120_000},
	}
}

// runtimeHeaderCursorContractJSON builds the exact Registry GraphQL response consumed by the Engine runtime client.
func runtimeHeaderCursorContractJSON(serviceID, versionID, endpointID uuid.UUID, baseURL string) string {
	return fmt.Sprintf(`{"data":{"serviceRuntimeContracts":[{
		"contract_version":2,
		"required_capabilities":["pagination.composable.v3"],
		"service_id":"%s",
		"service_version_id":"%s",
		"version":"v1",
		"service":{"id":"%s","current_service_version":"v1","name":"Parity","description":"","base_url":"%s","servers":[],"default_headers":{},"auth_configs":[],"rate_limit":null,"retry_config":null},
		"operations":[{"id":"%s","name":"listItems","method":"GET","path":"/items","normalized_path":"/items","provider_protocol":"rest","parameters":[{"name":"X-Page-Cursor","in":"header","required":false,"type":"string"}],"security_requirements":[{"schemes":[]}],"pagination":{"version":3,"request":[{"state":"cursor","target":{"location":"header","name":"X-Page-Cursor"},"value_type":"string","apply":"all"}],"response":{"items":{"path":"$.items"},"values":[{"name":"next_cursor","source":{"location":"header","name":"X-Next-Cursor","value_type":"string"}}]},"continuation":[{"kind":"token","state":"cursor","response_value":"next_cursor"}],"termination":{"stop_on_missing_values":["next_cursor"],"repeated_value":"error"},"limits":{"max_pages":100,"max_items":10000,"max_bytes":16777216,"max_duration_ms":120000}}}],
		"webhooks":[]
	}]}}`, serviceID, versionID, serviceID, baseURL, endpointID)
}

// assertRuntimeHeaderCursorAggregate verifies one response document contains both pages and retains first-page metadata.
func assertRuntimeHeaderCursorAggregate(t *testing.T, calls int, body []byte) {
	t.Helper()
	// Exactly two provider calls prove the decoded header cursor drove one continuation and then terminated.
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
	var document struct {
		Items  []int  `json:"items"`
		Source string `json:"source"`
	}
	// Canonical JSON decoding makes page aggregation assertions independent of field order.
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode aggregate: %v", err)
	}
	// First-page metadata remains authoritative while the items path accumulates both pages.
	if !reflect.DeepEqual(document.Items, []int{1, 2}) || document.Source != "canonical" {
		t.Fatalf("aggregate = %#v, body = %s", document, body)
	}
}
