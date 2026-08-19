package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type unifiedCompileStore struct {
	*workspaceTestStore
	calls    int
	requests []store.ServiceContractEndpointSelection
	matches  []store.ServiceContractEndpointMatch
}

// ListServiceContractEndpointsForSelections returns exact fixture data through the production SDK Unified compilation interface.
func (s *unifiedCompileStore) ListServiceContractEndpointsForSelections(_ context.Context, selections []store.ServiceContractEndpointSelection, _ []string) ([]store.ServiceContractEndpointMatch, error) {
	s.calls++
	s.requests = append([]store.ServiceContractEndpointSelection(nil), selections...)
	return append([]store.ServiceContractEndpointMatch(nil), s.matches...), nil
}

// TestCompileSDKUnifiedOperationsResolvesOnceAndPersistsExecutableDefinitions
// proves one snapshot query yields executable private programs while public
// descriptors omit every mapping expression.
func TestCompileSDKUnifiedOperationsResolvesOnceAndPersistsExecutableDefinitions(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatalf("compileSDKUnifiedOperations() error = %v", err)
	}
	assertUnifiedCompileRequests(t, fixture.store, [][]string{{"createTicket"}, {"createIssue"}})
	definition := mustDecodeSingleUnifiedDefinition(t, compiled.DefinitionJSON, 2)
	assertUnifiedMappedResult(t, definition, "@acme/custom-crm", map[string]any{"iid": "crm-1"}, "crm-1")
	assertUnifiedMappedResult(t, definition, "github", map[string]any{"id": "gh-1"}, "gh-1")
	assertUnifiedDescriptorOmits(t, compiled.Descriptors, "input.title", "response.github")
}

// TestCompileSDKUnifiedOperationsReusesAliasedServiceSelection proves graph
// aliases retain independent results while sharing one exact service selector.
func TestCompileSDKUnifiedOperationsReusesAliasedServiceSelection(t *testing.T) {
	fixture := newUnifiedAliasedCompileFixture()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	definition := mustDecodeSingleUnifiedDefinition(t, compiled.DefinitionJSON, 2)
	for index, binding := range definition.Bindings {
		if binding.ServiceTarget != "github" || binding.PublicTarget == "github" {
			t.Fatalf("binding %d = %#v", index, binding)
		}
		if fixture.store.requests[index].ServiceID != fixture.selections[1].ServiceID {
			t.Fatalf("request %d resolved the wrong service", index)
		}
		if compiled.Descriptors.Operations[0].Targets[index].ServiceTarget != "github" {
			t.Fatalf("descriptor target %d lost service alias", index)
		}
	}
	if fixture.store.calls != 1 {
		t.Fatalf("endpoint snapshot calls = %d, want 1", fixture.store.calls)
	}
}

// TestCompileSDKUnifiedRollbackResolvesOneBatchAndKeepsMappingPrivate proves
// forward and rollback identities share one query while private input stays local.
func TestCompileSDKUnifiedRollbackResolvesOneBatchAndKeepsMappingPrivate(t *testing.T) {
	fixture := newUnifiedRollbackCompileFixture()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	assertUnifiedCompileRequests(t, fixture.store, [][]string{{"createTicket"}, {"deleteTicket"}, {"createIssue"}})
	definition := mustDecodeSingleUnifiedDefinition(t, compiled.DefinitionJSON, 2)
	crm := definition.Bindings[0]
	if crm.Rollback == nil || crm.Rollback.OperationID != "deleteTicket" {
		t.Fatalf("compiled rollback = %#v", crm.Rollback)
	}
	github := definition.Bindings[1]
	if !reflect.DeepEqual(github.DependsOn, []string{"@acme/custom-crm"}) {
		t.Fatalf("compiled dependencies = %#v", github.DependsOn)
	}
	assertUnifiedDescriptorOmits(t, compiled.Descriptors, "response.@acme/custom-crm.iid")
}

// assertUnifiedCompileRequests requires one store call and stable flattened
// forward/rollback endpoint request order.
func assertUnifiedCompileRequests(t *testing.T, compileStore *unifiedCompileStore, wantNames [][]string) {
	t.Helper()
	if compileStore.calls != 1 {
		t.Fatalf("endpoint snapshot calls = %d, want 1", compileStore.calls)
	}
	if len(compileStore.requests) != len(wantNames) {
		t.Fatalf("endpoint snapshot requests = %d, want %d", len(compileStore.requests), len(wantNames))
	}
	for index, request := range compileStore.requests {
		if !reflect.DeepEqual(request.EndpointNames, wantNames[index]) {
			t.Fatalf("request %d endpoint names = %#v, want %#v", index, request.EndpointNames, wantNames[index])
		}
	}
}

// mustDecodeSingleUnifiedDefinition fails fixture setup unless canonical bytes
// contain exactly one operation and the requested number of bindings.
func mustDecodeSingleUnifiedDefinition(t *testing.T, raw []byte, bindingCount int) unified.OperationDefinition {
	t.Helper()
	definitions, err := unified.DecodeDefinitions(raw, unified.DefaultLimits())
	if err != nil {
		t.Fatalf("DecodeDefinitions() error = %v", err)
	}
	if len(definitions) != 1 || len(definitions[0].Bindings) != bindingCount {
		t.Fatalf("definitions = %#v", definitions)
	}
	return definitions[0]
}

// assertUnifiedDescriptorOmits scans serialized public descriptors for private
// expression fragments without decoding or normalizing them away.
func assertUnifiedDescriptorOmits(t *testing.T, descriptors *models.SDKUnifiedOperationDescriptors, forbidden ...string) {
	t.Helper()
	publicJSON, err := json.Marshal(descriptors)
	if err != nil {
		t.Fatalf("marshal descriptors: %v", err)
	}
	for _, value := range forbidden {
		if containsJSONText(publicJSON, value) {
			t.Fatalf("public descriptor leaked private mapping %q: %s", value, publicJSON)
		}
	}
}

// TestValidateGeneratedUnifiedTargetsRequiresRollbackEndpoint rejects a
// Registry package scope that omits the compiled compensation endpoint.
func TestValidateGeneratedUnifiedTargetsRequiresRollbackEndpoint(t *testing.T) {
	fixture := newUnifiedRollbackCompileFixture()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	returned := append([]models.SDKSelection(nil), fixture.selections...)
	returned[0].SelectAll = false
	returned[0].EndpointIDs = []uuid.UUID{fixture.store.matches[0].Endpoint.ID}
	returned[1].SelectAll = false
	returned[1].EndpointIDs = []uuid.UUID{fixture.store.matches[2].Endpoint.ID}
	if err := validateGeneratedUnifiedTargets(compiled.Descriptors, returned); err == nil {
		t.Fatal("validateGeneratedUnifiedTargets() accepted missing rollback endpoint")
	}
}

// TestCompileSDKUnifiedOperationsIsDeterministic protects the rule that equivalent configuration produces identical immutable bytes and hashes.
func TestCompileSDKUnifiedOperationsIsDeterministic(t *testing.T) {
	first := newUnifiedCompileFixture()
	second := newUnifiedCompileFixture()
	second.document.UnifiedOperations = map[string]sdkUnifiedOperationDoc{
		"issues.create": second.document.UnifiedOperations["issues.create"],
	}
	compiledFirst, err := compileSDKUnifiedOperations(context.Background(), first.store, first.document, first.selections, first.services)
	if err != nil {
		t.Fatal(err)
	}
	compiledSecond, err := compileSDKUnifiedOperations(context.Background(), second.store, second.document, second.selections, second.services)
	if err != nil {
		t.Fatal(err)
	}
	if compiledFirst.DefinitionHash != compiledSecond.DefinitionHash || compiledFirst.CodegenDescriptorHash != compiledSecond.CodegenDescriptorHash {
		t.Fatalf("whole-set hashes changed: %#v %#v", compiledFirst, compiledSecond)
	}
}

// TestCompileSDKUnifiedOperationsRejectsMissingExactEndpoint protects the rule that resolution uses exact service/version/endpoint identity instead of operation names alone.
func TestCompileSDKUnifiedOperationsRejectsMissingExactEndpoint(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	fixture.store.matches = fixture.store.matches[:1]
	_, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	assertWorkspaceErrorCode(t, err, "endpoint_not_found")
}

// TestCompileSDKUnifiedOperationsRejectsUndeclaredResponseDependency proves a
// mapping cannot read response data without the matching direct dependency edge.
func TestCompileSDKUnifiedOperationsRejectsUndeclaredResponseDependency(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	github := operation.Bindings["github"]
	github.Input = json.RawMessage(`{"id":"${response.@acme/custom-crm.iid}"}`)
	operation.Bindings["github"] = github
	fixture.document.UnifiedOperations["issues.create"] = operation
	_, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	assertWorkspaceErrorCode(t, err, "binding_input_invalid")
}

// TestNormalizeResolvedUnifiedPayloadRejectsTamperedDefinition proves the
// persisted private-definition hash detects post-plan mutation.
func TestNormalizeResolvedUnifiedPayloadRejectsTamperedDefinition(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	payload := appResolvedPayload{
		UnifiedDefinitionSchemaVersion: unified.DefinitionSchemaVersion,
		UnifiedDefinitions:             append(json.RawMessage(nil), compiled.DefinitionJSON...),
		UnifiedDefinitionHash:          compiled.DefinitionHash,
		UnifiedCodegenDescriptorHash:   compiled.CodegenDescriptorHash,
		UnifiedOperations:              compiled.Descriptors,
	}
	payload.UnifiedDefinitions[1] = ' '
	if err := normalizeAndValidateResolvedUnifiedPayload(&payload); err == nil {
		t.Fatal("normalizeAndValidateResolvedUnifiedPayload() accepted tampered definitions")
	}
}

// TestValidateGeneratedUnifiedTargetsRequiresEveryCompiledEndpoint protects the rule that resolution uses exact service/version/endpoint identity instead of operation names alone.
func TestValidateGeneratedUnifiedTargetsRequiresEveryCompiledEndpoint(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	returned := append([]models.SDKSelection(nil), fixture.selections...)
	returned[0].SelectAll = false
	returned[0].EndpointIDs = []uuid.UUID{fixture.store.matches[0].Endpoint.ID}
	returned[1].SelectAll = false
	returned[1].EndpointIDs = nil
	if err := validateGeneratedUnifiedTargets(compiled.Descriptors, returned); err == nil {
		t.Fatal("validateGeneratedUnifiedTargets() accepted a missing endpoint")
	}
}

type unifiedCompileFixture struct {
	store      *unifiedCompileStore
	document   sdkConfigDocument
	selections []models.SDKSelection
	services   []sdkResolvedService
}

// newUnifiedCompileFixture builds a deterministic SDK Unified compilation fixture with exact identities and isolated state.
func newUnifiedCompileFixture() unifiedCompileFixture {
	githubService := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	githubVersion := uuid.MustParse("11111111-1111-4111-8111-222222222222")
	githubEndpoint := uuid.MustParse("11111111-1111-4111-8111-333333333333")
	crmService := uuid.MustParse("22222222-2222-4222-8222-111111111111")
	crmVersion := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	crmEndpoint := uuid.MustParse("22222222-2222-4222-8222-333333333333")
	selections := []models.SDKSelection{
		{ServiceID: crmService, ServiceVersionID: crmVersion, SelectAll: true},
		{ServiceID: githubService, ServiceVersionID: githubVersion, OperationNames: []string{"createIssue"}},
	}
	services := []sdkResolvedService{
		{ServiceID: crmService, ServiceVersionID: crmVersion, PublicTarget: "@acme/custom-crm"},
		{ServiceID: githubService, ServiceVersionID: githubVersion, PublicTarget: "github"},
	}
	operation := sdkUnifiedOperationDoc{
		Input: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`),
		Bindings: map[string]sdkUnifiedBindingDoc{
			"github":           {Operation: "createIssue", Input: json.RawMessage(`{"title":"${input.title}"}`)},
			"@acme/custom-crm": {Operation: "createTicket", Input: json.RawMessage(`{"summary":"${input.title}"}`)},
		},
		Output: &sdkUnifiedOutputDoc{
			Schema:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
			Mapping: json.RawMessage(`{"id":"${response.github.id ?? response.@acme/custom-crm.iid}"}`),
		},
	}
	compileStore := &unifiedCompileStore{
		workspaceTestStore: &workspaceTestStore{},
		matches: []store.ServiceContractEndpointMatch{
			{SelectionIndex: 0, Endpoint: fusedobject.Endpoint{ID: crmEndpoint, Name: "createTicket"}},
			{SelectionIndex: 1, Endpoint: fusedobject.Endpoint{ID: githubEndpoint, Name: "createIssue"}},
		},
	}
	return unifiedCompileFixture{
		store: compileStore,
		document: sdkConfigDocument{
			Language: "python",
			Services: map[string]sdkConfigServiceDoc{
				"github":           {Operations: []string{"createIssue"}},
				"@acme/custom-crm": {SelectAll: true},
			},
			UnifiedOperations: map[string]sdkUnifiedOperationDoc{"issues.create": operation},
		},
		selections: selections,
		services:   services,
	}
}

// newUnifiedAliasedCompileFixture creates two graph steps backed by the same
// selected service so compile and runtime tests exercise selector reuse.
func newUnifiedAliasedCompileFixture() unifiedCompileFixture {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	operation.Bindings = map[string]sdkUnifiedBindingDoc{
		"github_create": {Service: "github", Operation: "createIssue", Input: json.RawMessage(`{"title":"${input.title}"}`)},
		"github_lookup": {Service: "github", Operation: "createIssue", Input: json.RawMessage(`{"title":"${input.title}"}`)},
	}
	operation.Output = nil
	fixture.document.UnifiedOperations["issues.create"] = operation
	fixture.store.matches = []store.ServiceContractEndpointMatch{
		{SelectionIndex: 0, Endpoint: fixture.store.matches[1].Endpoint},
		{SelectionIndex: 1, Endpoint: fixture.store.matches[1].Endpoint},
	}
	return fixture
}

// newUnifiedRollbackCompileFixture extends the base fixture with one direct
// dependency and a same-service compensation endpoint.
func newUnifiedRollbackCompileFixture() unifiedCompileFixture {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	crm := operation.Bindings["@acme/custom-crm"]
	crm.Rollback = &sdkUnifiedRollbackDoc{
		Operation: "deleteTicket",
		Input:     json.RawMessage(`{"ticket_id":"${response.@acme/custom-crm.iid}"}`),
	}
	operation.Bindings["@acme/custom-crm"] = crm
	github := operation.Bindings["github"]
	github.DependsOn = []string{"@acme/custom-crm"}
	github.Input = json.RawMessage(`{"title":"${input.title}","ticket_id":"${response.@acme/custom-crm.iid}"}`)
	operation.Bindings["github"] = github
	fixture.document.UnifiedOperations["issues.create"] = operation
	rollbackEndpoint := uuid.MustParse("22222222-2222-4222-8222-444444444444")
	fixture.store.matches = []store.ServiceContractEndpointMatch{
		fixture.store.matches[0],
		{SelectionIndex: 1, Endpoint: fusedobject.Endpoint{ID: rollbackEndpoint, Name: "deleteTicket"}},
		{SelectionIndex: 2, Endpoint: fixture.store.matches[1].Endpoint},
	}
	return fixture
}

// assertUnifiedMappedResult executes a persisted binding projection to prove
// canonical encoding did not change its target-specific response semantics.
func assertUnifiedMappedResult(t *testing.T, definition unified.OperationDefinition, target string, response map[string]any, want string) {
	t.Helper()
	got, err := definition.Output.Mapping.Evaluate(unified.EvaluationContext{Target: target, Response: response})
	if err != nil {
		t.Fatalf("Evaluate(%q) error = %v", target, err)
	}
	if got.(map[string]any)["id"] != want {
		t.Fatalf("Evaluate(%q) = %#v, want id %q", target, got, want)
	}
}

// containsJSONText supports byte-level leak assertions without interpreting private mapping data.
func containsJSONText(raw []byte, value string) bool {
	return len(raw) > 0 && json.Valid(raw) && stringContains(string(raw), value)
}

// stringContains supports byte-level leak assertions without interpreting private mapping data.
func stringContains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

// assertWorkspaceErrorCode requires compiler failures to retain their stable
// HTTP-facing classification instead of exposing implementation errors.
func assertWorkspaceErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	typed, ok := err.(workspaceConfigHTTPError)
	if !ok || typed.code != want {
		t.Fatalf("error = %#v, want code %q", err, want)
	}
}
