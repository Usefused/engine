package sandbox

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

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
	db := &mockCacheDB{contractMetadata: &fusedobject.ServiceMetadata{ID: svcA}, contractEndpoints: endpoints}
	cache := NewLocalObjectCache(db)

	selections := []models.SDKSelection{
		{ServiceID: svcA, ServiceVersionID: svcB, SelectAll: true},
	}

	fixture, err := buildSessionFixture(context.Background(), cache, selections, store.AppTokenPolicy{AllowAll: true})
	if err != nil {
		t.Fatalf("buildSessionFixture() error = %v", err)
	}

	if _, ok := fixture.Resolve("getUser"); !ok {
		t.Fatal(`fixture.Resolve("getUser") = not found, want found`)
	}
	assertListUsersOperation(t, fixture, svcA)
	if db.contractBatchCalls != 1 {
		t.Fatalf("unrestricted fixture snapshot queries = %d, want one batched query", db.contractBatchCalls)
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
	if op.OperationID != "listUsers" {
		t.Errorf("OperationID = %q, want %q", op.OperationID, "listUsers")
	}
	if op.ServiceID != wantServiceID.String() {
		t.Errorf("ServiceID = %q, want %q", op.ServiceID, wantServiceID.String())
	}
	if op.Method != "GET" || op.Path != "/users" {
		t.Errorf("Method/Path = %q/%q, want GET//users", op.Method, op.Path)
	}
	if len(op.Parameters) != 1 || op.Parameters[0].Name != "limit" {
		t.Errorf("Parameters = %+v, want one param named limit", op.Parameters)
	}
	if _, ok := op.Responses["200"]; !ok {
		t.Errorf("Responses = %+v, want a 200 entry", op.Responses)
	}
}

func TestBuildSessionFixture_PropagatesListEndpointsError(t *testing.T) {
	cache := NewLocalObjectCache(&mockCacheDB{contractErr: errTestRegistryUnavailable})

	selections := []models.SDKSelection{
		{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true},
	}
	_, err := buildSessionFixture(context.Background(), cache, selections, store.AppTokenPolicy{AllowAll: true})
	if err == nil {
		t.Fatal("buildSessionFixture() error = nil, want propagated error")
	}
}

func TestBuildSessionFixture_StrictTokenFetchesOnlyAllowedOperations(t *testing.T) {
	allowedGet := uuid.New()
	allowedList := uuid.New()
	db := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{ID: uuid.New(), Name: "Users"},
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

	fixture, err := buildSessionFixture(context.Background(), cache, selections, store.AppTokenPolicy{
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

func TestEndpointToFixtureOperation_SetsOperationIDAndServiceID(t *testing.T) {
	ep := fusedobject.Endpoint{
		ID:     uuid.New(),
		Name:   "listUsers",
		Method: "GET",
		Path:   "/users",
	}
	op, err := endpointToFixtureOperation("svc-123", ep)
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
