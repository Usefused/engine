package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

func testCursorPagination(name, path string) *fusedobject.PaginationConfig {
	return &fusedobject.PaginationConfig{
		Version: 2, Type: "cursor", ItemsPath: "$.items",
		Cursor: &paginationpolicy.CursorConfig{
			Request: paginationpolicy.RequestTarget{Location: "query", Name: name},
			Next:    paginationpolicy.ValueSource{Location: "body", Path: path, ValueType: "string"},
		},
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
		Type: "oauth2",
	}})

	if len(got) != 1 || got[0].Name != "" {
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

	if got.Pagination == nil || got.Pagination.Type != "cursor" {
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

	if got.Pagination == nil || got.Pagination.Type != "cursor" || got.Pagination.Cursor == nil || got.Pagination.Cursor.Request.Name != "after" || got.Pagination.Cursor.Next.Path != "$.page.next" {
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
		MediaType: "application/vnd.api+json", Serialization: fusedobject.RequestSerializationJSON,
		Required: true, Schema: &fusedobject.Schema{Type: "object", Required: []string{"id"}},
		PayloadParameter: "body", BinaryEncoding: fusedobject.RequestBinaryEncodingBase64,
		Parts: map[string]fusedobject.RequestPart{
			"file": {ContentType: "image/png", BinaryEncoding: fusedobject.RequestBinaryEncodingBase64},
		},
	}
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		RequestContent: requestContent,
	})

	if got.RequestContent == nil || got.RequestContent.MediaType != "application/vnd.api+json" || got.RequestContent.Serialization != "json" {
		t.Fatalf("expected request content to reach dispatcher model, got %#v", got.RequestContent)
	}
	if !got.RequestContent.Required || got.RequestContent.Schema == nil || got.RequestContent.Schema.Required[0] != "id" {
		t.Fatalf("request content schema was not preserved: %#v", got.RequestContent)
	}
	if got.RequestContent.PayloadParameter != "body" || got.RequestContent.BinaryEncoding != "base64" || got.RequestContent.Parts["file"].ContentType != "image/png" {
		t.Fatalf("request content wire metadata was not preserved: %#v", got.RequestContent)
	}
	got.RequestContent.Parts["file"] = models.RequestPart{ContentType: "changed"}
	if requestContent.Parts["file"].ContentType != "image/png" {
		t.Fatal("expected mapped parts to be independently owned")
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
			MediaType: "application/json", Serialization: fusedobject.RequestSerializationJSON,
			Schema: &fusedobject.Schema{Type: "object", Required: []string{"id"}},
		},
		Responses: fusedobject.Responses{"200": {Type: "object"}},
	}

	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, ep)

	if got.GraphQLQuery == nil || *got.GraphQLQuery != query {
		t.Fatalf("GraphQL query was not preserved: %#v", got.GraphQLQuery)
	}
	if got.ProviderProtocol != "graphql" || got.OperationKind != "query" {
		t.Fatalf("unexpected execution metadata: protocol=%q kind=%q", got.ProviderProtocol, got.OperationKind)
	}
	if len(got.Parameters) != 1 || got.RequestContent == nil || got.RequestContent.Schema == nil || got.Responses["200"].Type != "object" {
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

func TestFusedToIntegrationObject_RecognizesLegacyGraphQLSnapshot(t *testing.T) {
	query := `query Viewer { viewer { id } }`
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{
		GraphQLQuery: &query, ResourceName: "query",
	})

	if got.ProviderProtocol != "graphql" || got.OperationKind != "query" {
		t.Fatalf("legacy GraphQL snapshot was not upgraded: %#v", got)
	}
}
