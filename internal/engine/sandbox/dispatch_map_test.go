package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

func testCursorPagination(name, path string) *fusedobject.PaginationConfig {
	return &fusedobject.PaginationConfig{
		Version:      paginationpolicy.Version,
		Request:      []paginationpolicy.RequestStep{{State: "cursor", Target: paginationpolicy.RequestTarget{Location: paginationpolicy.RequestQuery, Name: name}, ValueType: paginationpolicy.ValueString, Apply: paginationpolicy.ApplyAll}},
		Response:     paginationpolicy.ResponsePlan{Items: paginationpolicy.ItemsSource{Path: "$.items"}, Values: []paginationpolicy.ResponseValue{{Name: "next", Source: paginationpolicy.ValueSource{Location: paginationpolicy.SourceBody, Path: path, ValueType: paginationpolicy.ValueString}}}},
		Continuation: []paginationpolicy.ContinuationStep{{Kind: paginationpolicy.ContinuationToken, State: "cursor", ResponseValue: "next"}},
		Termination:  paginationpolicy.Termination{StopOnEmptyItems: true, StopOnMissingValues: []string{"next"}, RepeatedValue: paginationpolicy.RepeatedStop},
	}
}

func TestMapAuthConfigsPreservesInvalidUnnamedBearerForValidation(t *testing.T) {
	got := mapAuthConfigs(fusedobject.AuthConfigs{{
		Type:   "http",
		Scheme: "bearer",
	}})

	if len(got) != 1 || got[0].Name != "" {
		t.Fatalf("unnamed bearer auth must not gain a legacy fallback: %#v", got)
	}
}

func TestMapAuthConfigsPreservesInvalidUnnamedOAuthForValidation(t *testing.T) {
	got := mapAuthConfigs(fusedobject.AuthConfigs{{
		Type:                    "oauth2",
		TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretBasic,
	}})

	if len(got) != 1 || got[0].Name != "" || got[0].TokenEndpointAuthMethod != models.TokenEndpointAuthMethodClientSecretBasic {
		t.Fatalf("unnamed oauth auth must not gain a legacy fallback: %#v", got)
	}
}

// TestFusedToIntegrationObject_EndpointPaginationWinsOverService pins the
// fallback rule from plans/plan-service-config-restructure.md item 1:
// spec-derived per-endpoint pagination always wins over the service/version
// default, since it's a more specific declaration.
func TestFusedToIntegrationObject_EndpointPaginationWinsOverService(t *testing.T) {
	svc := &fusedobject.ServiceMetadata{
		Pagination: testCursorPagination("service_cursor", "$.meta.next"),
	}
	ep := fusedobject.Endpoint{
		Pagination: testCursorPagination("cursor", "$.next"),
	}

	got := fusedToIntegrationObject(svc, ep)

	if got.Pagination == nil || got.Pagination.Request[0].Target.Name != "cursor" {
		t.Fatalf("expected endpoint pagination to win, got %#v", got.Pagination)
	}
}

// TestFusedToIntegrationObject_FallsBackToServicePagination covers the new
// behavior this item adds: an endpoint with no spec-derived pagination of
// its own now inherits the service/version-level execution_policy default
// instead of never auto-paginating.
func TestFusedToIntegrationObject_FallsBackToServicePagination(t *testing.T) {
	svc := &fusedobject.ServiceMetadata{
		Pagination: testCursorPagination("after", "$.page.next"),
	}
	ep := fusedobject.Endpoint{}

	got := fusedToIntegrationObject(svc, ep)

	if got.Pagination == nil || got.Pagination.Request[0].Target.Name != "after" || got.Pagination.Response.Values[0].Source.Path != "$.page.next" {
		t.Fatalf("expected fallback to service pagination, got %#v", got.Pagination)
	}
}

// TestFusedToIntegrationObject_NoPaginationAnywhereStaysNil confirms
// non-paginated endpoints on services with no declared default still dispatch
// without triggering the pagination path.
func TestFusedToIntegrationObject_NoPaginationAnywhereStaysNil(t *testing.T) {
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{})

	if got.Pagination != nil {
		t.Fatalf("expected nil pagination, got %#v", got.Pagination)
	}
}

func TestFusedToIntegrationObject_PreservesRequestContent(t *testing.T) {
	requestContent := &fusedobject.RequestContent{
		Required: true, PayloadParameter: "body", Representations: []fusedobject.RequestRepresentation{{
			MediaType: "application/vnd.api+json", Serialization: fusedobject.RequestSerializationJSON,
			Schema:   &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "object", Required: []string{"id"}}},
			Encoding: map[string]fusedobject.RequestEncoding{"file": {ContentType: "image/png", BinaryEncoding: fusedobject.RequestBinaryEncodingBase64}},
		}},
	}
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		RequestContent: requestContent,
	})
	assertMappedRequestContent(t, got.RequestContent)
	assertMappedRequestOwnership(t, requestContent, got.RequestContent)
}

func assertMappedRequestContent(t *testing.T, got *models.RequestContent) {
	t.Helper()
	if got == nil || len(got.Representations) != 1 || got.Representations[0].MediaType != "application/vnd.api+json" || got.Representations[0].Serialization != "json" {
		t.Fatalf("expected request content to reach dispatcher model, got %#v", got)
	}
	if !got.Required || got.Representations[0].Schema == nil || got.Representations[0].Schema.Projection.Required[0] != "id" {
		t.Fatalf("request content schema was not preserved: %#v", got)
	}
	if got.PayloadParameter != "body" || got.Representations[0].Encoding["file"].BinaryEncoding != "base64" || got.Representations[0].Encoding["file"].ContentType != "image/png" {
		t.Fatalf("request content wire metadata was not preserved: %#v", got)
	}
}

func assertMappedRequestOwnership(t *testing.T, source *fusedobject.RequestContent, got *models.RequestContent) {
	t.Helper()
	got.Representations[0].Encoding["file"] = models.RequestEncoding{ContentType: "changed"}
	if source.Representations[0].Encoding["file"].ContentType != "image/png" {
		t.Fatal("expected mapped encodings to be independently owned")
	}
}

func TestFusedToServicePreservesTagHierarchy(t *testing.T) {
	source := &fusedobject.ServiceMetadata{Documentation: &fusedobject.ServiceDocumentation{
		Tags: []fusedobject.TagDocumentation{{
			Name: "orders", Summary: "Order operations", Description: "Order APIs",
			Parent: "commerce", Kind: "badge",
		}},
	}}

	got := fusedToService(source)
	if got.Documentation == nil || len(got.Documentation.Tags) != 1 {
		t.Fatalf("mapped service documentation = %#v", got.Documentation)
	}
	tag := got.Documentation.Tags[0]
	if tag.Name != "orders" || tag.Summary != "Order operations" || tag.Description != "Order APIs" || tag.Parent != "commerce" || tag.Kind != "badge" {
		t.Fatalf("tag hierarchy metadata was lost: %#v", tag)
	}
}

func TestFusedToIntegrationObjectPreservesFullMediaContracts(t *testing.T) {
	raw := []byte(`{"oneOf":[{"type":"string"},{"type":"null"}]}`)
	contract := &fusedobject.SchemaContract{
		Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw, ContentHash: "hash",
		Projection: fusedobject.Schema{Type: "string"},
	}
	ep := fusedobject.Endpoint{
		RequestContent: &fusedobject.RequestContent{
			DefaultMediaType: "application/vnd.test+json",
			Representations: []fusedobject.RequestRepresentation{{
				MediaType: "application/vnd.test+json", Serialization: fusedobject.RequestSerializationJSON, Schema: contract,
				Encoding: map[string]fusedobject.RequestEncoding{"file": {ContentType: "image/png", BinaryEncoding: "base64"}},
			}},
		},
		Responses: fusedobject.Responses{"2XX": {
			Description: "ok", Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/vnd.test+json", Schema: contract}},
		}},
	}
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, ep)
	representation := got.RequestContent.Representations[0]
	if got.RequestContent.DefaultMediaType != "application/vnd.test+json" || string(representation.Schema.Raw) != string(raw) || representation.Schema.Projection.Type != "string" {
		t.Fatalf("request representation lost fidelity: %#v", got.RequestContent)
	}
	if got.Responses["2XX"].Representations[0].MediaType != "application/vnd.test+json" {
		t.Fatalf("response representation lost fidelity: %#v", got.Responses)
	}
	raw[0] = '['
	if string(representation.Schema.Raw)[0] != '{' {
		t.Fatal("mapped raw schema aliases Registry snapshot memory")
	}
}

func TestFusedToIntegrationObject_PreservesPathEncoding(t *testing.T) {
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		Parameters: fusedobject.Parameters{{
			Name: "resource", In: "path", PathEncoding: fusedobject.PathEncodingPreserveSlashes,
		}},
	})
	if len(got.Parameters) != 1 || got.Parameters[0].PathEncoding != models.PathEncodingPreserveSlashes {
		t.Fatalf("path encoding was not preserved: %#v", got.Parameters)
	}
}

func TestMapSchema_PreservesAdditionalPropertiesValueSchema(t *testing.T) {
	source := &fusedobject.Schema{
		Type: "object", AdditionalProperties: &fusedobject.Schema{Type: "string", Format: "uuid"},
	}
	mapped := mapSchema(source)
	if mapped == nil || mapped.AdditionalProperties == nil || mapped.AdditionalProperties.Type != "string" || mapped.AdditionalProperties.Format != "uuid" {
		t.Fatalf("additional properties schema was not preserved: %#v", mapped)
	}
	mapped.AdditionalProperties.Format = "changed"
	if source.AdditionalProperties.Format != "uuid" {
		t.Fatal("expected additional properties schema to be independently owned")
	}
}

func TestFusedToIntegrationObject_PreservesGraphQLExecutionContract(t *testing.T) {
	query := `query Viewer($id: ID!) { viewer(id: $id) { id } }`
	ep := fusedobject.Endpoint{
		Name: "viewer", Method: "POST", Path: "/graphql", GraphQLQuery: &query,
		ProviderProtocol: "graphql", OperationKind: "query",
		Parameters: fusedobject.Parameters{{Name: "id", Required: true, Type: "string"}},
		RequestContent: &fusedobject.RequestContent{
			Representations: []fusedobject.RequestRepresentation{{MediaType: "application/json", Serialization: fusedobject.RequestSerializationJSON,
				Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "object", Required: []string{"id"}}}}},
		},
		Responses: fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "object"}}}}}},
	}

	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, ep)

	if got.GraphQLQuery == nil || *got.GraphQLQuery != query {
		t.Fatalf("GraphQL query was not preserved: %#v", got.GraphQLQuery)
	}
	if got.ProviderProtocol != "graphql" || got.OperationKind != "query" {
		t.Fatalf("unexpected execution metadata: protocol=%q kind=%q", got.ProviderProtocol, got.OperationKind)
	}
	if len(got.Parameters) != 1 || got.RequestContent == nil || got.RequestContent.Representations[0].Schema == nil || got.Responses["200"].Representations[0].Schema.Projection.Type != "object" {
		t.Fatalf("operation schemas were not preserved: %#v", got)
	}
}

func TestFusedToIntegrationObject_TransportsStableKeyVerbatim(t *testing.T) {
	stableKey := "rest:GET:/drive/v3/files/{}"
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		Name: "getFile", Method: "get", NormalizedPath: "/different/path", StableKey: stableKey,
	})
	if got.StableKey != stableKey {
		t.Fatalf("stable key = %q, want Registry value %q", got.StableKey, stableKey)
	}
}

func TestFusedToIntegrationObject_DoesNotInferCanonicalExecutionFields(t *testing.T) {
	query := `query Viewer { viewer { id } }`
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		GraphQLQuery: &query, ResourceName: "query",
		Parameters: fusedobject.Parameters{{
			Name: "id", In: "query", Type: "string",
			Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "integer"}},
		}},
	})

	if got.ProviderProtocol != "" || got.OperationKind != "" || got.Parameters[0].Type != "" {
		t.Fatalf("legacy execution fields were inferred: %#v", got)
	}
}

func TestFusedToIntegrationObject_PreservesBaselineParameterType(t *testing.T) {
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		Parameters: fusedobject.Parameters{{Name: "limit", In: "query", Type: "integer"}},
	})
	if len(got.Parameters) != 1 || got.Parameters[0].Type != "integer" {
		t.Fatalf("baseline parameter type was not preserved: %#v", got.Parameters)
	}
}
