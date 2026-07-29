package sandbox

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestListEndpointsForSelection_SelectAllReturnsEverything(t *testing.T) {
	epA := uuid.New()
	epB := uuid.New()
	rc := &mockRegistryClient{
		serviceOperations: []fusedobject.Endpoint{
			{ID: epA, Name: "listUsers"},
			{ID: epB, Name: "getUser"},
		},
	}
	cache := NewLocalObjectCache(&mockCacheDB{}, rc)

	sel := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true}
	got, err := cache.ListEndpointsForSelection(context.Background(), "sdk-1", sel)
	if err != nil {
		t.Fatalf("ListEndpointsForSelection() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (SelectAll should return every endpoint)", len(got))
	}
}

func TestListEndpointsForSelection_FiltersByEndpointIDs(t *testing.T) {
	wanted := uuid.New()
	other := uuid.New()
	rc := &mockRegistryClient{
		serviceOperations: []fusedobject.Endpoint{
			{ID: wanted, Name: "listUsers"},
			{ID: other, Name: "deleteUser"},
		},
	}
	cache := NewLocalObjectCache(&mockCacheDB{}, rc)

	sel := models.SDKSelection{
		ServiceID:        uuid.New(),
		ServiceVersionID: uuid.New(),
		EndpointIDs:      []uuid.UUID{wanted},
	}
	got, err := cache.ListEndpointsForSelection(context.Background(), "sdk-1", sel)
	if err != nil {
		t.Fatalf("ListEndpointsForSelection() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "listUsers" {
		t.Fatalf("got = %+v, want only the endpoint named in EndpointIDs", got)
	}
}

func TestListEndpointsForSelection_UsesSnapshotIDFilter(t *testing.T) {
	wanted := uuid.New()
	other := uuid.New()
	db := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{ID: uuid.New(), Name: "SnapshotService"},
		contractEndpoints: []fusedobject.Endpoint{
			{ID: wanted, Name: "listUsers"},
			{ID: other, Name: "deleteUser"},
		},
	}
	rc := &mockRegistryClient{
		serviceOperations: []fusedobject.Endpoint{{ID: other, Name: "registryOnly"}},
	}
	cache := NewLocalObjectCache(db, rc)

	sel := models.SDKSelection{
		ServiceID:        uuid.New(),
		ServiceVersionID: uuid.New(),
		EndpointIDs:      []uuid.UUID{wanted},
	}
	got, err := cache.ListEndpointsForSelection(context.Background(), "sdk-1", sel)
	if err != nil {
		t.Fatalf("ListEndpointsForSelection() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != wanted {
		t.Fatalf("got = %+v, want only the snapshot endpoint selected by id", got)
	}
	if db.contractIDCalls != 1 || rc.serviceOperationsCount != 0 {
		t.Fatalf("expected one snapshot ID query and no registry list, got snapshot=%d registry=%d", db.contractIDCalls, rc.serviceOperationsCount)
	}
}

func TestListEndpointsForSelection_EmptyEndpointIDsGrantsNothing(t *testing.T) {
	// A non-SelectAll selection with no EndpointIDs is a scope with no
	// endpoints, not a wildcard -- must not silently fall back to "everything".
	rc := &mockRegistryClient{
		serviceOperations: []fusedobject.Endpoint{{ID: uuid.New(), Name: "listUsers"}},
	}
	cache := NewLocalObjectCache(&mockCacheDB{}, rc)

	sel := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New()}
	got, err := cache.ListEndpointsForSelection(context.Background(), "sdk-1", sel)
	if err != nil {
		t.Fatalf("ListEndpointsForSelection() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

func TestListEndpointsForSelection_RegistryErrorPropagates(t *testing.T) {
	rc := &mockRegistryClient{serviceOperationsErr: errTestRegistryUnavailable}
	cache := NewLocalObjectCache(&mockCacheDB{}, rc)

	sel := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true}
	_, err := cache.ListEndpointsForSelection(context.Background(), "sdk-1", sel)
	if err == nil {
		t.Fatal("ListEndpointsForSelection() error = nil, want propagated registry error")
	}
}

func TestBuildSessionFixture_MapsEndpointsAcrossSelections(t *testing.T) {
	svcA := uuid.New()
	svcB := uuid.New()
	epA := uuid.New()
	epB := uuid.New()

	rc := &mockRegistryClient{}
	cache := NewLocalObjectCache(&mockCacheDB{}, rc)

	// Different registry responses per service aren't representable by the
	// shared mockRegistryClient's single serviceOperations field, so this
	// test uses one selection with SelectAll -- enough to exercise the
	// mapping (OperationID/ServiceID set, schema fields carried through)
	// without needing a per-service-keyed fake.
	rc.serviceOperations = []fusedobject.Endpoint{
		{
			ID: epA, Name: "listUsers", Description: "List users", Method: "GET", Path: "/users",
			Parameters: fusedobject.Parameters{{Name: "limit", In: "query", Type: "integer"}},
			Responses:  fusedobject.Responses{"200": {Type: "object"}},
		},
		{ID: epB, Name: "getUser", Method: "GET", Path: "/users/{id}"},
	}

	selections := []models.SDKSelection{
		{ServiceID: svcA, ServiceVersionID: svcB, SelectAll: true},
	}

	fixture, err := buildSessionFixture(context.Background(), cache, "sdk-1", selections)
	if err != nil {
		t.Fatalf("buildSessionFixture() error = %v", err)
	}

	if _, ok := fixture.Resolve("getUser"); !ok {
		t.Fatal(`fixture.Resolve("getUser") = not found, want found`)
	}
	assertListUsersOperation(t, fixture, svcA)
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
	rc := &mockRegistryClient{serviceOperationsErr: errTestRegistryUnavailable}
	cache := NewLocalObjectCache(&mockCacheDB{}, rc)

	selections := []models.SDKSelection{
		{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true},
	}
	_, err := buildSessionFixture(context.Background(), cache, "sdk-1", selections)
	if err == nil {
		t.Fatal("buildSessionFixture() error = nil, want propagated error")
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
