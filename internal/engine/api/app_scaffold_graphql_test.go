package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type appScaffoldGraphQLStore struct {
	store.Store
	resolved      []store.AppScaffoldResolvedSelection
	metadata      map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata
	endpoints     []store.ServiceContractEndpointMatch
	overrides     map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride
	scope         accesscontrol.AuthorizedScope
	resolveCalls  int
	metadataCalls int
	endpointCalls int
	policyCalls   int
}

// ResolveAuthorizedAppScaffoldSelections captures the authorization scope and
// emulates the store's fail-closed empty-scope behavior.
func (s *appScaffoldGraphQLStore) ResolveAuthorizedAppScaffoldSelections(_ context.Context, scope accesscontrol.AuthorizedScope, _ []store.AppScaffoldSelectionRef) ([]store.AppScaffoldResolvedSelection, error) {
	s.resolveCalls++
	s.scope = scope
	// An actor without service.read cannot resolve authoring labels to UUIDs.
	if !scope.All && len(scope.IDs) == 0 {
		return nil, store.ErrAppScaffoldSelectionUnavailable
	}
	return s.resolved, nil
}

// ListServiceContractMetadata records the single metadata batch call.
func (s *appScaffoldGraphQLStore) ListServiceContractMetadata(_ context.Context, _ []store.ServiceContractMetadataRef) (map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	s.metadataCalls++
	return s.metadata, nil
}

// ListServiceContractEndpointsForSelections records the single SQL-selected
// endpoint batch call without broad catalogue filtering in the mock.
func (s *appScaffoldGraphQLStore) ListServiceContractEndpointsForSelections(_ context.Context, _ []store.ServiceContractEndpointSelection, _ []string) ([]store.ServiceContractEndpointMatch, error) {
	s.endpointCalls++
	return s.endpoints, nil
}

// GetEffectiveWorkspaceExecutionPolicyOverrides records the single effective
// policy batch call used to satisfy workspace-provided variables.
func (s *appScaffoldGraphQLStore) GetEffectiveWorkspaceExecutionPolicyOverrides(_ context.Context, _ []store.WorkspaceExecutionPolicyRef) (map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride, error) {
	s.policyCalls++
	return s.overrides, nil
}

// TestAppScaffoldRequirementsGraphQLProjectsAuthorizedBatch verifies GraphQL
// coercion, set-based repository use, precedence, ordering, and safe telemetry.
func TestAppScaffoldRequirementsGraphQLProjectsAuthorizedBatch(t *testing.T) {
	alphaService, alphaVersion := uuid.New(), uuid.New()
	zetaService, zetaVersion := uuid.New(), uuid.New()
	fixture := &appScaffoldGraphQLStore{
		resolved: []store.AppScaffoldResolvedSelection{
			{SelectionIndex: 1, ServiceKey: "alpha", ServiceID: alphaService, ServiceVersionID: alphaVersion},
			{SelectionIndex: 0, ServiceKey: "zeta", ServiceID: zetaService, ServiceVersionID: zetaVersion},
		},
		metadata: map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata{
			{ServiceID: alphaService, ServiceVersionID: alphaVersion}: scaffoldMetadata("account_id"),
			{ServiceID: zetaService, ServiceVersionID: zetaVersion}:   scaffoldMetadata("app_id"),
		},
		endpoints: []store.ServiceContractEndpointMatch{
			{SelectionIndex: 0, Endpoint: scaffoldEndpoint("region")},
			{SelectionIndex: 1, Endpoint: scaffoldEndpointWithNoVariables()},
		},
		overrides: map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride{
			{ServiceID: zetaService, ServiceVersionID: zetaVersion}: {ServerVariables: map[string]string{"region": "eu"}},
		},
	}
	schema, err := newMCPGraphQLSchema(nil, fixture, nil, nil, nil)
	// A missing authorization policy would make schema construction fail closed.
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema: %v", err)
	}
	exporter := setupTestTracer(t)
	actor := actorWithResourcePermissions(t, uuid.New(),
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: alphaService}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: zetaService}},
	)
	body := `{"query":"query($selections:[AppScaffoldSelectionInput!]!){ appScaffoldRequirements(selections:$selections){ service variable } }","variables":{"selections":[{"service":"zeta","version":"v1","operations":["send"],"select_all":false},{"service":"alpha","version":"v2","operations":["read"],"select_all":false}]}}`
	response := executeAppScaffoldGraphQL(t, schema, fixture, actor, body)
	var payload struct {
		Data struct {
			Requirements []appScaffoldRequirement `json:"appScaffoldRequirements"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	// Decoding through JSON asserts the public field names rather than Go reflection.
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v: %s", err, response.Body.String())
	}
	want := []appScaffoldRequirement{{Service: "alpha", Variable: "account_id"}, {Service: "zeta", Variable: "app_id"}}
	// The response must remain deterministic even when the store returns reversed rows.
	if response.Code != http.StatusOK || len(payload.Errors) != 0 || !reflect.DeepEqual(payload.Data.Requirements, want) {
		t.Fatalf("status/body = %d/%s, want %#v", response.Code, response.Body.String(), want)
	}
	// Every repository surface is invoked once for the complete batch.
	if fixture.resolveCalls != 1 || fixture.metadataCalls != 1 || fixture.endpointCalls != 1 || fixture.policyCalls != 1 {
		t.Fatalf("batch calls = resolve:%d metadata:%d endpoints:%d policies:%d", fixture.resolveCalls, fixture.metadataCalls, fixture.endpointCalls, fixture.policyCalls)
	}
	// Resource-scoped authorization is carried into the UUID resolution query.
	if fixture.scope.All || len(fixture.scope.IDs) != 2 {
		t.Fatalf("authorized scope = %#v", fixture.scope)
	}
	assertAppScaffoldTelemetryIsCountOnly(t, exporter.GetSpans())
}

// TestAppScaffoldRequirementsGraphQLFailsClosedWithoutServiceRead proves a
// collection query never promotes a workspace-authenticated actor to services.
func TestAppScaffoldRequirementsGraphQLFailsClosedWithoutServiceRead(t *testing.T) {
	fixture := &appScaffoldGraphQLStore{}
	schema, err := newMCPGraphQLSchema(nil, fixture, nil, nil, nil)
	// Schema construction is a test precondition rather than the behavior under test.
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema: %v", err)
	}
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionWorkspaceRead)
	body := `{"query":"query { appScaffoldRequirements(selections:[{service:\"sendbird\",version:\"v1\",operations:[\"send\"],select_all:false}]) { service variable } }"}`
	response := executeAppScaffoldGraphQL(t, schema, fixture, actor, body)

	// The authorization planner rejects the request before any resolver or store call.
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), string(accesscontrol.PermissionServiceRead)) {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	// Authorization failure keeps every identity, snapshot, and policy read at zero.
	if fixture.resolveCalls != 0 || fixture.metadataCalls != 0 || fixture.endpointCalls != 0 || fixture.policyCalls != 0 {
		t.Fatalf("batch calls = resolve:%d metadata:%d endpoints:%d policies:%d", fixture.resolveCalls, fixture.metadataCalls, fixture.endpointCalls, fixture.policyCalls)
	}
}

// TestDecodeAppScaffoldSelectionsEnforcesBatchBounds keeps oversized authoring
// input from reaching SQL even when every individual scalar is valid.
func TestDecodeAppScaffoldSelectionsEnforcesBatchBounds(t *testing.T) {
	items := make([]interface{}, store.MaxAppScaffoldSelections+1)
	// Every item is scalar-valid; the aggregate cap is checked before deduplication.
	for index := range items {
		items[index] = map[string]interface{}{"service": strings.Repeat("s", index%10+1), "version": "v1", "select_all": true}
	}
	// The request is rejected before any store adapter is involved.
	if _, err := decodeAppScaffoldSelections(items); err == nil {
		t.Fatal("expected oversized scaffold selection batch to fail")
	}
}

// TestAppScaffoldExplicitOperationNamesPreservesSelectAll prevents stale
// operation lists from narrowing the authoritative all-operations selection.
func TestAppScaffoldExplicitOperationNamesPreservesSelectAll(t *testing.T) {
	selection := appScaffoldSelection{SelectAll: true, Operations: []string{"staleOperation"}}
	// Select-all is represented by a nil SQL name filter, while explicit mode retains exact names.
	if names := appScaffoldExplicitOperationNames(selection); names != nil {
		t.Fatalf("select-all names = %#v, want nil", names)
	}
	selection.SelectAll = false
	// Explicit mode must preserve the already-validated operation list.
	if names := appScaffoldExplicitOperationNames(selection); !reflect.DeepEqual(names, selection.Operations) {
		t.Fatalf("explicit names = %#v, want %#v", names, selection.Operations)
	}
}

// scaffoldMetadata builds the smallest executable default server contract for
// GraphQL projection tests.
func scaffoldMetadata(variable string) *fusedobject.ServiceMetadata {
	return &fusedobject.ServiceMetadata{Servers: fusedobject.Servers{{
		URL: "https://{{" + variable + "}}.example.com", IsDefault: true,
		Variables: []serverrouting.Variable{{Name: variable}},
	}}}
}

// scaffoldEndpoint adds one selected operation-server variable to exercise
// workspace policy satisfaction.
func scaffoldEndpoint(variable string) fusedobject.Endpoint {
	return fusedobject.Endpoint{Name: "send", OperationServers: fusedobject.Servers{{
		URL: "https://{{" + variable + "}}.example.com", IsDefault: true,
		Variables: []serverrouting.Variable{{Name: variable}},
	}}}
}

// scaffoldEndpointWithNoVariables supplies the explicit second operation
// without adding another unresolved routing input.
func scaffoldEndpointWithNoVariables() fusedobject.Endpoint {
	return fusedobject.Endpoint{Name: "read"}
}

// executeAppScaffoldGraphQL runs through the production authorization handler
// so resolver scope behavior is covered rather than calling graphql.Do directly.
func executeAppScaffoldGraphQL(t *testing.T, schema graphql.Schema, fixture store.Store, actor accesscontrol.Actor, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: fixture})(response, request)
	return response
}

// assertAppScaffoldTelemetryIsCountOnly ensures the new span contains no
// service, version, operation, variable, URL, or connection-reference fields.
func assertAppScaffoldTelemetryIsCountOnly(t *testing.T, spans []tracetest.SpanStub) {
	t.Helper()
	allowed := map[attribute.Key]bool{
		"engine.app_scaffold.selection_count":           true,
		"engine.app_scaffold.operation_count":           true,
		"engine.app_scaffold.resolved_service_count":    true,
		"engine.app_scaffold.unresolved_variable_count": true,
	}
	// Only the target resolver span is subject to this feature's telemetry contract.
	for _, span := range spans {
		// Other GraphQL and authorization spans retain their independent schemas.
		if span.Name != "engine.graphql.app_scaffold_requirements" {
			continue
		}
		// Any non-count attribute would create a new accidental data channel.
		for _, value := range span.Attributes {
			if !allowed[value.Key] {
				t.Fatalf("unsafe scaffold telemetry attribute %q", value.Key)
			}
		}
		return
	}
	t.Fatal("app scaffold resolver span not found")
}
