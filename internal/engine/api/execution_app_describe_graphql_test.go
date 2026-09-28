package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type executionBuildSelectionResult struct {
	Service          string `json:"service"`
	Operation        string `json:"operation"`
	ServiceID        string `json:"serviceId"`
	ServiceVersionID string `json:"serviceVersionId"`
	EndpointID       string `json:"endpointId"`
}

// TestExecutionBuildSelectionsReturnsExactAuthorizedIDs verifies the compiler
// receives pinned IDs through two batch reads even when store rows are reversed.
func TestExecutionBuildSelectionsReturnsExactAuthorizedIDs(t *testing.T) {
	alphaService, alphaVersion, alphaEndpoint := uuid.New(), uuid.New(), uuid.New()
	zetaService, zetaVersion, zetaEndpoint := uuid.New(), uuid.New(), uuid.New()
	fixture := &appScaffoldGraphQLStore{
		resolved: []store.AppScaffoldResolvedSelection{
			{SelectionIndex: 1, ServiceKey: "alpha", ServiceID: alphaService, ServiceVersionID: alphaVersion},
			{SelectionIndex: 0, ServiceKey: "zeta", ServiceID: zetaService, ServiceVersionID: zetaVersion},
		},
		endpoints: []store.ServiceContractEndpointMatch{
			{SelectionIndex: 1, Endpoint: fusedobject.Endpoint{ID: alphaEndpoint, Name: "read"}},
			{SelectionIndex: 0, Endpoint: fusedobject.Endpoint{ID: zetaEndpoint, Name: "send"}},
		},
	}
	schema, err := newMCPGraphQLSchema(nil, fixture, nil, nil, nil, nil, nil)
	// A schema without an explicit field policy must fail closed at setup time.
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema: %v", err)
	}
	exporter := setupTestTracer(t)
	actor := actorWithResourcePermissions(t, uuid.New(),
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: alphaService}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: zetaService}},
	)
	body := `{"query":"query($selections:[AppScaffoldSelectionInput!]!){ executionBuildSelections(selections:$selections){ service operation serviceId serviceVersionId endpointId } }","variables":{"selections":[{"service":"zeta","version":"v1","operations":["send"],"select_all":false},{"service":"alpha","version":"v2","operations":["read"],"select_all":false}]}}`
	response := executeAppScaffoldGraphQL(t, schema, fixture, actor, body)
	want := []executionBuildSelectionResult{
		{Service: "zeta", Operation: "send", ServiceID: zetaService.String(), ServiceVersionID: zetaVersion.String(), EndpointID: zetaEndpoint.String()},
		{Service: "alpha", Operation: "read", ServiceID: alphaService.String(), ServiceVersionID: alphaVersion.String(), EndpointID: alphaEndpoint.String()},
	}
	assertExecutionBuildResponse(t, response, want)
	assertExecutionBuildBatch(t, fixture)
	assertExecutionBuildTelemetry(t, exporter.GetSpans())
}

// assertExecutionBuildResponse validates the public wire names and authored
// order instead of relying on internal resolver types.
func assertExecutionBuildResponse(t *testing.T, response *httptest.ResponseRecorder, want []executionBuildSelectionResult) {
	t.Helper()
	var payload struct {
		Data struct {
			Selections []executionBuildSelectionResult `json:"executionBuildSelections"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	// A malformed response is an API regression even if the resolver returned no Go error.
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v: %s", err, response.Body.String())
	}
	// Ordered projections allow the compiler to compare a reviewed spec with its bundle manifest.
	if response.Code != http.StatusOK || len(payload.Errors) != 0 || !reflect.DeepEqual(payload.Data.Selections, want) {
		t.Fatalf("status/body = %d/%s, want %#v", response.Code, response.Body.String(), want)
	}
}

// assertExecutionBuildBatch catches accidental per-operation reads and any
// authorization filtering moved from SQL into Go.
func assertExecutionBuildBatch(t *testing.T, fixture *appScaffoldGraphQLStore) {
	t.Helper()
	// Each SQL selection surface is called once for the whole request, without metadata or policy reads.
	if fixture.resolveCalls != 1 || fixture.endpointCalls != 1 || fixture.metadataCalls != 0 || fixture.policyCalls != 0 {
		t.Fatalf("batch calls = resolve:%d endpoints:%d metadata:%d policies:%d", fixture.resolveCalls, fixture.endpointCalls, fixture.metadataCalls, fixture.policyCalls)
	}
	// The authorized service IDs reach the store rather than being filtered after a broad read.
	if fixture.scope.All || len(fixture.scope.IDs) != 2 {
		t.Fatalf("authorized scope = %#v", fixture.scope)
	}
	// The endpoint query itself carries each service's exact operation scope.
	if len(fixture.endpointInputs) != 2 || !reflect.DeepEqual(fixture.endpointInputs[0].EndpointNames, []string{"send"}) || !reflect.DeepEqual(fixture.endpointInputs[1].EndpointNames, []string{"read"}) {
		t.Fatalf("endpoint batch inputs = %#v", fixture.endpointInputs)
	}
}

// assertExecutionBuildTelemetry ensures debugging captures only an operation
// count and never adds service, endpoint, or version identifiers to traces.
func assertExecutionBuildTelemetry(t *testing.T, spans []tracetest.SpanStub) {
	t.Helper()
	for _, span := range spans {
		// Other resolver spans have their own telemetry contracts.
		if span.Name != "engine.graphql.execution_build_selections" {
			continue
		}
		// A new attribute needs an explicit review before operation identity enters telemetry.
		if !reflect.DeepEqual(span.Attributes, []attribute.KeyValue{attribute.Int("execution.build_operation_count", 2)}) {
			t.Fatalf("execution build span attributes = %#v", span.Attributes)
		}
		return
	}
	t.Fatal("execution build resolver span not found")
}

// TestExecutionBuildSelectionsRejectsUnauthorizedAccess keeps read permission
// enforcement in front of both bounded database queries.
func TestExecutionBuildSelectionsRejectsUnauthorizedAccess(t *testing.T) {
	fixture := &appScaffoldGraphQLStore{}
	schema, err := newMCPGraphQLSchema(nil, fixture, nil, nil, nil, nil, nil)
	// Schema construction is only the test setup prerequisite.
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema: %v", err)
	}
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionWorkspaceRead)
	body := `{"query":"query { executionBuildSelections(selections:[{service:\"alpha\",version:\"v1\",operations:[\"read\"],select_all:false}]) { endpointId } }"}`
	response := executeAppScaffoldGraphQL(t, schema, fixture, actor, body)
	// Denial happens in the GraphQL policy before the resolver can read identity data.
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), string(accesscontrol.PermissionServiceRead)) {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	// No endpoint or service read may occur on an unauthorized request.
	if fixture.resolveCalls != 0 || fixture.endpointCalls != 0 {
		t.Fatalf("batch calls = resolve:%d endpoints:%d", fixture.resolveCalls, fixture.endpointCalls)
	}
}

// TestExecutionBuildSelectionsRejectsUnpinnableSelections ensures a mutable
// select-all catalogue and an empty operation set cannot produce a bundle.
func TestExecutionBuildSelectionsRejectsUnpinnableSelections(t *testing.T) {
	for _, operations := range [][]string{nil, {"read"}} {
		selection := map[string]interface{}{"service": "alpha", "version": "v1", "select_all": operations != nil}
		items := make([]interface{}, 0, len(operations))
		// Explicit operation names are passed through the same GraphQL coercion shape.
		for _, operation := range operations {
			items = append(items, operation)
		}
		selection["operations"] = items
		// Both invalid modes are rejected before any store adapter is called.
		if _, err := decodeExecutionBuildSelections([]interface{}{selection}); err == nil {
			t.Fatalf("selection %#v unexpectedly accepted", selection)
		}
	}
}

// TestExecutionBuildSelectionsRejectsOversizedManifest keeps generated code
// within the same fixed operation count that bundle attach can inspect.
func TestExecutionBuildSelectionsRejectsOversizedManifest(t *testing.T) {
	operations := make([]interface{}, maxExecutionAppSelectedOperations+1)
	// Unique operation names prevent deduplication from hiding aggregate size.
	for index := range operations {
		operations[index] = "operation_" + strconv.Itoa(index)
	}
	selection := map[string]interface{}{"service": "alpha", "version": "v1", "select_all": false, "operations": operations}
	// The cap applies before any service identity or endpoint snapshot can be read.
	if _, err := decodeExecutionBuildSelections([]interface{}{selection}); err == nil {
		t.Fatal("oversized execution build selection unexpectedly accepted")
	}
}
