package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestBuildSessionFixture_MapsEndpointsAcrossSelections verifies one batched catalogue carries current effective policy.
func TestBuildSessionFixture_MapsEndpointsAcrossSelections(t *testing.T) {
	svcA := uuid.New()
	svcB := uuid.New()
	epA := uuid.New()
	epB := uuid.New()

	endpoints := []fusedobject.Endpoint{
		{
			ID: epA, Name: "listUsers", Description: "List users", Method: "GET", Path: "/users",
			Parameters: fusedobject.Parameters{{Name: "limit", In: "query", Type: "integer"}},
			Responses:  fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "object"}}}}}},
		},
		{ID: epB, Name: "getUser", Method: "GET", Path: "/users/{id}"},
	}
	servicePolicy := paginationGuidancePolicy(10)
	overridePolicy := paginationGuidancePolicy(5)
	db := &mockCacheDB{
		contractMetadata:  &fusedobject.ServiceMetadata{ID: svcA, Pagination: servicePolicy},
		contractEndpoints: endpoints,
		appName:           "directory-assistant", appVersion: "2.1.0",
		appDescription: "Find and review people in the connected directory.",
		policyOverrides: map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride{
			{ServiceID: svcA, ServiceVersionID: svcB}: {Pagination: overridePolicy},
		},
	}
	cache := NewLocalObjectCache(db)

	selections := []models.SDKSelection{
		{ServiceID: svcA, ServiceVersionID: svcB, SelectAll: true},
	}
	seedMCPPaginationMetadata(cache, selections, db.contractMetadata)

	fixture, err := buildSessionFixture(context.Background(), cache, uuid.NewString(), selections, store.AppTokenPolicy{AllowAll: true})
	if err != nil {
		t.Fatalf("buildSessionFixture() error = %v", err)
	}

	if _, ok := fixture.Resolve("getUser"); !ok {
		t.Fatal(`fixture.Resolve("getUser") = not found, want found`)
	}
	assertListUsersOperation(t, fixture, svcA)
	// Session identity must come from the same immutable app version as the selected catalogue.
	if fixture.Server.Name != db.appName || fixture.Server.Version != db.appVersion || fixture.Server.Description != db.appDescription {
		t.Fatalf("fixture server metadata = %#v", fixture.Server)
	}
	if db.contractBatchCalls != 1 {
		t.Fatalf("unrestricted fixture snapshot queries = %d, want one batched query", db.contractBatchCalls)
	}
	// Effective workspace overrides must remain one set-based lookup for the entire selection.
	if db.policyBatchCalls != 1 {
		t.Fatalf("pagination policy queries = %d, want one batched query", db.policyBatchCalls)
	}
}

// TestValidateMCPServerMetadataRejectsMissingIdentity prevents generic compatibility servers from starting.
func TestValidateMCPServerMetadataRejectsMissingIdentity(t *testing.T) {
	_, err := validateMCPServerMetadata(FixtureServerMetadata{})
	// Missing authored prose must make the immutable version unrunnable.
	if err == nil {
		t.Fatal("missing MCP server metadata was accepted")
	}
}

// assertListUsersOperation checks the "listUsers" operation's fields, split
// out of TestBuildSessionFixture_MapsEndpointsAcrossSelections to keep that
// test's own branching low.
func assertListUsersOperation(t *testing.T, fixture *Fixture, wantServiceID uuid.UUID) {
	t.Helper()
	op, ok := fixture.Resolve("listUsers")
	if !ok {
		t.Fatal(`fixture.Resolve("listUsers") = not found, want found`)
	}
	assertListUsersIdentity(t, op, wantServiceID)
	assertListUsersContract(t, op)
}

// assertListUsersIdentity checks the exact public dispatch identity independently from contract detail.
func assertListUsersIdentity(t *testing.T, op *FixtureOperation, wantServiceID uuid.UUID) {
	t.Helper()
	if op.OperationID != "listUsers" {
		t.Errorf("OperationID = %q, want %q", op.OperationID, "listUsers")
	}
	if op.ServiceID != wantServiceID.String() {
		t.Errorf("ServiceID = %q, want %q", op.ServiceID, wantServiceID.String())
	}
	if op.Method != "GET" || op.Path != "/users" {
		t.Errorf("Method/Path = %q/%q, want GET//users", op.Method, op.Path)
	}
}

// assertListUsersContract checks request, response, and effective pagination projections together.
func assertListUsersContract(t *testing.T, op *FixtureOperation) {
	t.Helper()
	if len(op.Parameters) != 1 || op.Parameters[0].Name != "limit" {
		t.Errorf("Parameters = %+v, want one param named limit", op.Parameters)
	}
	if _, ok := op.Responses["200"]; !ok {
		t.Errorf("Responses = %+v, want a 200 entry", op.Responses)
	}
	// Discovery must expose the current workspace reduction rather than cached service defaults.
	if !op.Pagination.Supported || !op.Pagination.CallerBoundSupported || op.Pagination.EngineMaxPages != 5 {
		t.Errorf("Pagination = %+v, want current workspace limit 5", op.Pagination)
	}
}

func TestBuildSessionFixture_PropagatesListEndpointsError(t *testing.T) {
	cache := NewLocalObjectCache(&mockCacheDB{contractErr: errTestRegistryUnavailable})

	selections := []models.SDKSelection{
		{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true},
	}
	_, err := buildSessionFixture(context.Background(), cache, uuid.NewString(), selections, store.AppTokenPolicy{AllowAll: true})
	if err == nil {
		t.Fatal("buildSessionFixture() error = nil, want propagated error")
	}
}

// TestBuildSessionFixture_StrictTokenFetchesOnlyAllowedOperations keeps token filtering and policy loading set-based.
func TestBuildSessionFixture_StrictTokenFetchesOnlyAllowedOperations(t *testing.T) {
	allowedGet := uuid.New()
	allowedList := uuid.New()
	db := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{ID: uuid.New(), Name: "Users"},
		appName:          "users-test", appVersion: "1.0.0", appDescription: "Find and manage test users.",
		contractEndpoints: []fusedobject.Endpoint{
			{ID: allowedGet, Name: "getUser", Method: "GET", Path: "/users/{id}"},
			{ID: allowedList, Name: "listUsers", Method: "GET", Path: "/users"},
			{ID: uuid.New(), Name: "deleteUser", Method: "DELETE", Path: "/users/{id}"},
		}}
	cache := NewLocalObjectCache(db)
	selections := []models.SDKSelection{
		{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointIDs: []uuid.UUID{allowedGet}},
		{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointIDs: []uuid.UUID{allowedList}},
	}
	seedMCPPaginationMetadata(cache, selections, db.contractMetadata)

	fixture, err := buildSessionFixture(context.Background(), cache, uuid.NewString(), selections, store.AppTokenPolicy{
		AllowedOperations: []string{"getUser", "listUsers", "deleteUser"},
	})
	if err != nil {
		t.Fatalf("buildSessionFixture() error = %v", err)
	}
	if _, ok := fixture.Resolve("getUser"); !ok {
		t.Fatal("allowed operation is missing from strict token fixture")
	}
	if _, ok := fixture.Resolve("listUsers"); !ok {
		t.Fatal("allowed operation from the second selection is missing")
	}
	if _, ok := fixture.Resolve("deleteUser"); ok {
		t.Fatal("operation outside the app selection leaked into the strict fixture")
	}
	if db.contractBatchCalls != 1 {
		t.Fatalf("strict fixture snapshot queries = %d, want one batched query", db.contractBatchCalls)
	}
}

// seedMCPPaginationMetadata mirrors the metadata prewarm completed before a live MCP fixture is built.
func seedMCPPaginationMetadata(cache *LocalObjectCache, selections []models.SDKSelection, metadata *fusedobject.ServiceMetadata) {
	// Each immutable selection receives its own copy so test mutations cannot alias policy state.
	for _, selection := range selections {
		copyValue := *metadata
		copyValue.ID = selection.ServiceID
		copyValue.ServiceVersionID = selection.ServiceVersionID
		cache.serviceMetadataCache[selection.ServiceID.String()+":"+selection.ServiceVersionID.String()] = &copyValue
	}
}

// TestBuildSessionFixtureAttachesAppliedPlanDescriptor verifies the session
// uses the store-owned public descriptor without changing physical lookup.
func TestBuildSessionFixtureAttachesAppliedPlanDescriptor(t *testing.T) {
	database := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{ID: uuid.New(), Name: "Users"},
		appName:          "users-sync-test", appVersion: "1.0.0", appDescription: "Synchronize test users.",
		unifiedDescriptors: &models.SDKUnifiedOperationDescriptors{
			SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion,
			Operations:    []models.SDKUnifiedOperationDescriptor{{Name: "users.sync", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		},
	}
	fixture, err := buildSessionFixture(context.Background(), NewLocalObjectCache(database), uuid.NewString(), nil, store.AppTokenPolicy{AllowAll: true})
	if err != nil {
		t.Fatalf("buildSessionFixture() error = %v", err)
	}
	if fixture.UnifiedOperations == nil || len(fixture.UnifiedOperations.Operations) != 1 || fixture.UnifiedOperations.Operations[0].Name != "users.sync" {
		t.Fatal("Unified descriptor was not attached under its exact authored name")
	}
	// One descriptor read per session keeps query count constant as the logical
	// operation count grows and avoids a new process cache.
	if database.unifiedCalls != 1 {
		t.Fatalf("Unified descriptor calls = %d, want 1", database.unifiedCalls)
	}
}

func TestNewFixtureFromOperations_DedupesDuplicateOperationID(t *testing.T) {
	ops := []FixtureOperation{
		{OperationID: "listUsers", ServiceID: "svc-a"},
		{OperationID: "listUsers", ServiceID: "svc-b"}, // duplicate name, different service
		{OperationID: "getUser", ServiceID: "svc-a"},
	}

	fixture := newFixtureFromOperations(context.Background(), ops)

	if len(fixture.Operations) != 2 {
		t.Fatalf("len(Operations) = %d, want 2 (duplicate should be dropped, not both kept)", len(fixture.Operations))
	}
	op, ok := fixture.Resolve("listUsers")
	if !ok {
		t.Fatal(`fixture.Resolve("listUsers") = not found, want found`)
	}
	if op.ServiceID != "svc-a" {
		t.Errorf("ServiceID = %q, want %q (first occurrence should win)", op.ServiceID, "svc-a")
	}
}

// TestEndpointToFixtureOperation_SetsOperationIDAndServiceID verifies public identity and policy survive conversion together.
func TestEndpointToFixtureOperation_SetsOperationIDAndServiceID(t *testing.T) {
	ep := fusedobject.Endpoint{
		ID:         uuid.New(),
		Name:       "listUsers",
		Method:     "GET",
		Path:       "/users",
		Pagination: paginationGuidancePolicy(8),
	}
	op, err := endpointToFixtureOperation("svc-123", ep, nil)
	if err != nil {
		t.Fatalf("endpointToFixtureOperation() error = %v", err)
	}
	if op.OperationID != "listUsers" {
		t.Errorf("OperationID = %q, want %q", op.OperationID, "listUsers")
	}
	if op.ServiceID != "svc-123" {
		t.Errorf("ServiceID = %q, want %q", op.ServiceID, "svc-123")
	}
	if op.Name != "listUsers" || op.Method != "GET" || op.Path != "/users" {
		t.Errorf("op = %+v, want Name/Method/Path carried through from the endpoint", op)
	}
	// Endpoint policy has execution precedence and must survive fixture projection unchanged.
	if !op.Pagination.Supported || !op.Pagination.CallerBoundSupported || op.Pagination.EngineMaxPages != 8 {
		t.Errorf("Pagination = %+v, want supported Engine limit 8", op.Pagination)
	}
}

func TestStripMCPAuthParametersKeepsAuthOutOfSearchDocs(t *testing.T) {
	operation := FixtureOperation{Parameters: []models.Parameter{
		{Name: "Authorization", In: "header"},
		{Name: "X-API-Key", In: "header"},
		{Name: "project_id", In: "query"},
		{Name: "fused_end_user_ref", In: "query"},
	}}
	stripMCPAuthParameters(&operation, "X-API-Key")
	if len(operation.Parameters) != 1 || operation.Parameters[0].Name != "project_id" {
		t.Fatalf("auth parameters leaked into fixture: %#v", operation.Parameters)
	}
}

// errTestRegistryUnavailable is a stand-in Registry error for tests that only
// care that an error propagates, not its exact message.
var errTestRegistryUnavailable = errRegistryUnavailableForTest{}

type errRegistryUnavailableForTest struct{}

func (errRegistryUnavailableForTest) Error() string { return "registry unavailable (test)" }
