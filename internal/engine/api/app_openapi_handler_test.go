package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

type appOpenAPITestStore struct {
	store.Store
	app         *store.App
	family      *store.AppFamily
	matches     []store.ServiceContractEndpointMatch
	queryNames  []string
	contractErr error
}

// GetApp returns only the exact fixture app identity.
func (s *appOpenAPITestStore) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	if s.app == nil || s.app.AppID != appID {
		return nil, store.ErrAppNotFound
	}
	copy := *s.app
	return &copy, nil
}

// GetAppFamily returns the fixture family so handler scope checks remain real.
func (s *appOpenAPITestStore) GetAppFamily(_ context.Context, _ uuid.UUID) (*store.AppFamily, error) {
	if s.family == nil {
		return nil, store.ErrAppFamilyNotFound
	}
	copy := *s.family
	return &copy, nil
}

// ListServiceContractEndpointsForSelections records the pushed operation
// allowlist and models the production query's exact-name intersection.
func (s *appOpenAPITestStore) ListServiceContractEndpointsForSelections(_ context.Context, _ []store.ServiceContractEndpointSelection, names []string) ([]store.ServiceContractEndpointMatch, error) {
	s.queryNames = append([]string(nil), names...)
	if s.contractErr != nil || len(names) == 0 {
		return append([]store.ServiceContractEndpointMatch(nil), s.matches...), s.contractErr
	}
	filtered := make([]store.ServiceContractEndpointMatch, 0)
	for _, match := range s.matches {
		if match.Endpoint.Name == names[0] {
			filtered = append(filtered, match)
		}
	}
	return filtered, nil
}

// newAppOpenAPIFixture builds one runnable exact SDK app with an immutable
// physical selection and integrity-checked empty Unified definition set.
func newAppOpenAPIFixture(t *testing.T) (*appOpenAPITestStore, fusedobject.Endpoint) {
	t.Helper()
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	endpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "createIssue", Responses: fusedobject.Responses{}}
	selections, err := json.Marshal([]models.SDKSelection{{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), DefinitionSchemaVersion: models.SDKDefinitionSchemaVersion,
		EndpointIDs: []uuid.UUID{endpoint.ID}, OperationNames: []string{endpoint.Name},
	}})
	if err != nil {
		t.Fatalf("encode selections: %v", err)
	}
	return &appOpenAPITestStore{
		app: &store.App{
			AppID: appID, AppFamilyID: familyID, AccountID: accountID, Version: "1.2.3",
			ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections, Status: store.AppStatusActive,
			UnifiedDefinitionSchemaVersion: unified.DefinitionSchemaVersion, UnifiedDefinitions: []byte("[]"), UnifiedDefinitionHash: store.EmptyUnifiedSetHash,
		},
		family:  &store.AppFamily{AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindSDK, DisplayName: "Issue app"},
		matches: []store.ServiceContractEndpointMatch{{SelectionIndex: 0, Endpoint: endpoint}},
	}, endpoint
}

// mountAppOpenAPITestHandler mounts the exact handler with an authenticated
// control-plane actor for the fixture workspace.
func mountAppOpenAPITestHandler(s *appOpenAPITestStore) http.Handler {
	router := newControlTestRouter(s.app.AccountID)
	router.Get("/apps/{app_id}/openapi", AppOpenAPIHandler(s, s))
	return router
}

// TestAppOpenAPIHandlerExportsValidFlatOpenAPI verifies OpenAPI 3.1 validity,
// runtime-flat composed body declarations, and bounded public output shapes.
func TestAppOpenAPIHandlerExportsValidFlatOpenAPI(t *testing.T) {
	s, endpoint := newAppOpenAPIFixture(t)
	endpoint.Parameters = fusedobject.Parameters{{Name: "project", Required: true, Type: "string"}}
	endpoint.RequestContent = composedOpenAPIRequestContent()
	endpoint.Responses = fusedobject.Responses{"201": {Representations: []fusedobject.ResponseRepresentation{{
		MediaType: "application/json", Schema: rawOpenAPISchema(`{"type":"object","properties":{"id":{"type":"string"}}}`),
	}}}}
	s.matches[0].Endpoint = endpoint

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi", nil)
	mountAppOpenAPITestHandler(s).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	document, err := openapi3.NewLoader().LoadFromData(recorder.Body.Bytes())
	if err != nil {
		t.Fatalf("load OpenAPI: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate OpenAPI: %v", err)
	}
	assertFlatOpenAPIInput(t, recorder.Body.Bytes(), endpoint.Name)
	if recorder.Header().Get("Cache-Control") != "no-store" || strings.Contains(recorder.Body.String(), "provider-private-operation") {
		t.Fatalf("response headers or secrecy projection are invalid")
	}
}

// composedOpenAPIRequestContent creates a root-ref/allOf schema that must be
// exposed as flat execution input rather than a synthetic body property.
func composedOpenAPIRequestContent() *fusedobject.RequestContent {
	return &fusedobject.RequestContent{Required: true, Representations: []fusedobject.RequestRepresentation{{
		MediaType: "application/json", Serialization: fusedobject.RequestSerializationJSON,
		Schema:   rawOpenAPISchema(`{"$ref":"#/$defs/body","$defs":{"body":{"allOf":[{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]},{"type":"object","properties":{"priority":{"type":"integer"}},"additionalProperties":{"type":"string"}}]}}}`),
		Encoding: map[string]fusedobject.RequestEncoding{"attachment": {}},
	}}}
}

// rawOpenAPISchema constructs a canonical raw schema contract for fixtures.
func rawOpenAPISchema(raw string) *fusedobject.SchemaContract {
	return &fusedobject.SchemaContract{Raw: json.RawMessage(raw)}
}

// assertFlatOpenAPIInput inspects the generated request branch for exact flat
// fields, required projection, additional schema, and reserved control field.
func assertFlatOpenAPIInput(t *testing.T, payload []byte, operation string) {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode generated document: %v", err)
	}
	request := openAPITestOperationRequest(t, document, operation)
	properties := request["properties"].(map[string]any)["input"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"project", "title", "priority", "attachment", "_headers"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("flat input is missing %q: %#v", name, properties)
		}
	}
	if _, synthetic := properties["body"]; synthetic {
		t.Fatalf("composed object body was exposed as a synthetic body property")
	}
	input := request["properties"].(map[string]any)["input"].(map[string]any)
	additional := input["additionalProperties"].(map[string]any)
	if additional["type"] != "string" || !containsAllStrings(stringSlice(input["required"]), "project", "title") {
		t.Fatalf("flat input requirements/additional schema are invalid: %#v", input)
	}
}

// openAPITestOperationRequest resolves a generated operation request component.
func openAPITestOperationRequest(t *testing.T, document map[string]any, operation string) map[string]any {
	t.Helper()
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	request, ok := components[openAPIOperationComponentKey(operation)+"Request"].(map[string]any)
	if !ok {
		t.Fatalf("request component for %q is unavailable", operation)
	}
	return request
}

// containsAllStrings reports whether every expected string is present.
func containsAllStrings(values []string, expected ...string) bool {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range expected {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

// TestAppOpenAPIOperationFilterPushesDownBeforeCollision proves an unrelated
// whole-app collision cannot block an exact bounded export.
func TestAppOpenAPIOperationFilterPushesDownBeforeCollision(t *testing.T) {
	s, wanted := newAppOpenAPIFixture(t)
	duplicateID := uuid.New()
	s.matches = append(s.matches,
		store.ServiceContractEndpointMatch{SelectionIndex: 0, Endpoint: fusedobject.Endpoint{ID: duplicateID, Name: "unrelated"}},
		store.ServiceContractEndpointMatch{SelectionIndex: 0, Endpoint: fusedobject.Endpoint{ID: uuid.New(), Name: "unrelated"}},
	)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi?operation="+wanted.Name, nil)
	mountAppOpenAPITestHandler(s).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(s.queryNames) != 1 || s.queryNames[0] != wanted.Name {
		t.Fatalf("status/filter = %d/%v, body = %s", recorder.Code, s.queryNames, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "unrelated") {
		t.Fatalf("filtered export includes an unrelated operation")
	}
}

// TestAppOpenAPIHandlerRejectsExactPhysicalUnifiedCollision verifies request
// shape cannot disambiguate an immutable same-name collision.
func TestAppOpenAPIHandlerRejectsExactPhysicalUnifiedCollision(t *testing.T) {
	s, endpoint := newAppOpenAPIFixture(t)
	s.app.UnifiedDefinitions = encodeAppOpenAPITestDefinitions(t, endpoint.Name)
	s.app.UnifiedDefinitionHash = mustAppOpenAPIHash(t, s.app.UnifiedDefinitions)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi?operation="+endpoint.Name, nil)
	mountAppOpenAPITestHandler(s).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestAppOpenAPIHandlerProjectsUnifiedPublicContract verifies service-keyed
// selectors, explicit targets, private-definition secrecy, and result types.
func TestAppOpenAPIHandlerProjectsUnifiedPublicContract(t *testing.T) {
	s, _ := newAppOpenAPIFixture(t)
	operation := "searchAndEmail"
	s.app.UnifiedDefinitions = encodeAppOpenAPITestDefinitions(t, operation)
	s.app.UnifiedDefinitionHash = mustAppOpenAPIHash(t, s.app.UnifiedDefinitions)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi?operation="+operation, nil)
	mountAppOpenAPITestHandler(s).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "provider-private-operation") {
		t.Fatalf("private Unified provider identity leaked into public OpenAPI")
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	requestSchema := openAPITestOperationRequest(t, document, operation)
	requestProperties := requestSchema["properties"].(map[string]any)
	assertUnifiedOpenAPIRouting(t, requestProperties)
	response := openAPITestOperationResponse(t, document, operation)
	assertUnifiedOpenAPIResultTypes(t, response)
}

// assertUnifiedOpenAPIRouting checks target and service selector namespaces.
func assertUnifiedOpenAPIRouting(t *testing.T, properties map[string]any) {
	t.Helper()
	targets := properties["targets"].(map[string]any)
	items := targets["items"].(map[string]any)
	if !containsAllStrings(stringSlice(items["enum"]), "issue") || targets["minItems"] != float64(1) {
		t.Fatalf("Unified targets are invalid: %#v", targets)
	}
	selectors := properties["selectors"].(map[string]any)["properties"].(map[string]any)
	if _, ok := selectors["jira"]; !ok {
		t.Fatalf("Unified service selector is missing: %#v", selectors)
	}
}

// openAPITestOperationResponse resolves a generated operation response component.
func openAPITestOperationResponse(t *testing.T, document map[string]any, operation string) map[string]any {
	t.Helper()
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	response, ok := components[openAPIOperationComponentKey(operation)+"Response"].(map[string]any)
	if !ok {
		t.Fatalf("response component for %q is unavailable", operation)
	}
	return response
}

// assertUnifiedOpenAPIResultTypes verifies generated schemas match the actual
// optional string/object fields and repeated rollback trigger wire field.
func assertUnifiedOpenAPIResultTypes(t *testing.T, response map[string]any) {
	t.Helper()
	properties := response["properties"].(map[string]any)
	result := properties["results"].(map[string]any)["items"].(map[string]any)
	resultProperties := result["properties"].(map[string]any)
	if resultProperties["error_code"].(map[string]any)["type"] != "string" || result["additionalProperties"] != false {
		t.Fatalf("Unified result schema is not exact: %#v", result)
	}
	rollback := properties["rollbacks"].(map[string]any)["items"].(map[string]any)
	triggeredBy := rollback["properties"].(map[string]any)["triggered_by"].(map[string]any)
	if triggeredBy["type"] != "array" || triggeredBy["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("rollback triggered_by schema is invalid: %#v", triggeredBy)
	}
}

// encodeAppOpenAPITestDefinitions creates one valid private Unified definition.
func encodeAppOpenAPITestDefinitions(t *testing.T, name string) []byte {
	t.Helper()
	encoded, err := unified.EncodeDefinitions([]unified.OperationDefinition{{
		Name: name, InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		Bindings: []unified.BindingDefinition{{
			PublicTarget: "issue", ServiceTarget: "jira", OperationID: "provider-private-operation",
			ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointID: uuid.New(),
		}},
	}}, unified.DefaultLimits())
	if err != nil {
		t.Fatalf("encode Unified definitions: %v", err)
	}
	return encoded
}

// mustAppOpenAPIHash computes the production canonical definition hash.
func mustAppOpenAPIHash(t *testing.T, payload []byte) string {
	t.Helper()
	hash, err := unifiedCanonicalHash(payload)
	if err != nil {
		t.Fatalf("hash Unified definitions: %v", err)
	}
	return hash
}

// TestAppOpenAPIHandlerEnforcesExactSDKScope covers account, family-kind, and
// runnable-version checks before immutable schemas are returned.
func TestAppOpenAPIHandlerEnforcesExactSDKScope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*appOpenAPITestStore)
		status int
	}{
		{name: "cross account", mutate: func(s *appOpenAPITestStore) { s.app.AccountID = uuid.New() }, status: http.StatusForbidden},
		{name: "MCP family", mutate: func(s *appOpenAPITestStore) { s.family.Kind = store.AppKindMCP }, status: http.StatusBadRequest},
		{name: "wrong family", mutate: func(s *appOpenAPITestStore) { s.family.AppFamilyID = uuid.New() }, status: http.StatusBadRequest},
		{name: "building", mutate: func(s *appOpenAPITestStore) { s.app.Status = store.AppStatusBuilding }, status: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newAppOpenAPIFixture(t)
			accountID := s.app.AccountID
			test.mutate(s)
			router := newControlTestRouter(accountID)
			router.Get("/apps/{app_id}/openapi", AppOpenAPIHandler(s, s))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi", nil))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, test.status, recorder.Body.String())
			}
		})
	}
}

// TestAppOpenAPIHandlerRequiresAuthenticatedActor proves the export cannot be
// used as an app-existence oracle outside the control-plane authorization path.
func TestAppOpenAPIHandlerRequiresAuthenticatedActor(t *testing.T) {
	s, _ := newAppOpenAPIFixture(t)
	recorder := httptest.NewRecorder()
	AppOpenAPIHandler(s, s).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestAppOpenAPIHandlerRejectsUncallablePhysicalOperationName keeps generated
// operations within the exact direct REST execution name contract.
func TestAppOpenAPIHandlerRejectsUncallablePhysicalOperationName(t *testing.T) {
	s, _ := newAppOpenAPIFixture(t)
	name := strings.Repeat("x", maxRESTOperationBytes+1)
	s.matches[0].Endpoint.Name = name
	selections := []models.SDKSelection{{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), DefinitionSchemaVersion: models.SDKDefinitionSchemaVersion,
		EndpointIDs: []uuid.UUID{s.matches[0].Endpoint.ID}, OperationNames: []string{name},
	}}
	s.app.Selections = mustJSON(t, selections)
	recorder := httptest.NewRecorder()
	mountAppOpenAPITestHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestBuildAppOpenAPIDocumentEnforcesResponseCap proves oversized whole-app
// exports fail with the documented operation-filter guidance.
func TestBuildAppOpenAPIDocumentEnforcesResponseCap(t *testing.T) {
	s, endpoint := newAppOpenAPIFixture(t)
	large := strings.Repeat("x", maxAppOpenAPIBytes)
	endpoint.Responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{
		MediaType: "application/json", Schema: rawOpenAPISchema(`{"type":"string","description":` + string(mustJSON(t, large)) + `}`),
	}}}}
	s.matches[0].Endpoint = endpoint
	_, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "")
	httpErr, ok := err.(workspaceConfigHTTPError)
	if !ok || httpErr.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("error = %#v, want response cap error", err)
	}
}

// mustJSON returns one JSON encoding for a fixture value.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return encoded
}

// TestAppOpenAPIHandlerRejectsCorruptEmptyUnifiedSet proves an encoded empty
// definition array still crosses the immutable schema/hash integrity boundary.
func TestAppOpenAPIHandlerRejectsCorruptEmptyUnifiedSet(t *testing.T) {
	s, _ := newAppOpenAPIFixture(t)
	s.app.UnifiedDefinitionHash = "sha256:wrong"
	recorder := httptest.NewRecorder()
	mountAppOpenAPITestHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apps/"+s.app.AppID.String()+"/openapi", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestProjectionOpenAPIRequiredStrings verifies the reviewed projection's
// native []string required list survives flat body projection.
func TestProjectionOpenAPIRequiredStrings(t *testing.T) {
	endpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "projection", RequestContent: &fusedobject.RequestContent{
		Representations: []fusedobject.RequestRepresentation{{Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{
			Type: "object", Required: []string{"name"}, Properties: map[string]fusedobject.Schema{"name": {Type: "string"}},
		}}}},
	}}
	input, err := physicalOpenAPIInputSchema(endpoint)
	if err != nil || !containsAllStrings(stringSlice(input["required"]), "name") {
		t.Fatalf("input/error = %#v/%v, required projection was lost", input, err)
	}
}

// TestPhysicalOpenAPIInputRejectsReservedHeadersName proves the generated
// control field never overwrites a provider-declared parameter or body field.
func TestPhysicalOpenAPIInputRejectsReservedHeadersName(t *testing.T) {
	parameter := fusedobject.Endpoint{ID: uuid.New(), Name: "parameter", Parameters: fusedobject.Parameters{{Name: "_headers"}}}
	body := fusedobject.Endpoint{ID: uuid.New(), Name: "body", RequestContent: &fusedobject.RequestContent{Representations: []fusedobject.RequestRepresentation{{
		Schema: rawOpenAPISchema(`{"type":"object","properties":{"_headers":{"type":"string"}}}`),
	}}}}
	for _, endpoint := range []fusedobject.Endpoint{parameter, body} {
		if _, err := physicalOpenAPIInputSchema(endpoint); err == nil {
			t.Fatalf("reserved _headers declaration was accepted for %s", endpoint.Name)
		}
	}
}
