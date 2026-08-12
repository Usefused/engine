package sandbox

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

func TestRuntimeContractOperationQueryIncludesExecutionFields(t *testing.T) {
	for _, field := range []string{"operation_servers", "serialization", "allow_reserved", "allow_empty_value", "content", "examples"} {
		if !strings.Contains(runtimeContractOperationFields, field) {
			t.Fatalf("runtime contract query is missing %q", field)
		}
	}
	if !strings.Contains(runtimeContractsQuery, "oauth2_metadata_url") {
		t.Fatal("runtime contract query does not select OAuth2 metadata URL")
	}
	if !strings.Contains(runtimeContractsQuery, "servers {\n\t\turl\n\t\tname") ||
		!strings.Contains(runtimeContractOperationFields, "operation_servers {\n\t\turl\n\t\tname") {
		t.Fatal("runtime contract query does not select OpenAPI 3.2 server names")
	}
}

func TestValidateEndpointTransportAcceptsBoundedMethods(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE", "QUERY", "COPY"} {
		if err := validateEndpointTransport(fusedobject.Endpoint{Method: method, ProviderProtocol: models.ProviderProtocolREST}); err != nil {
			t.Fatalf("method %s: %v", method, err)
		}
	}
	for _, method := range []string{"", "BAD METHOD", "GET\r\nX-Evil", strings.Repeat("X", 33)} {
		if err := validateEndpointTransport(fusedobject.Endpoint{Method: method}); err == nil {
			t.Fatalf("invalid method %q accepted", method)
		}
	}
}

func TestValidateEndpointTransportRejectsInvalidParameterContracts(t *testing.T) {
	falseValue := false
	schema := operationTestSchemaContract()
	tests := []struct {
		name       string
		parameters fusedobject.Parameters
	}{
		{name: "duplicate", parameters: fusedobject.Parameters{{Name: "q", In: "query", Schema: schema}, {Name: "q", In: "query", Schema: schema}}},
		{name: "optional path", parameters: fusedobject.Parameters{{Name: "id", In: "path", Schema: schema}}},
		{name: "schema and content", parameters: fusedobject.Parameters{{Name: "q", In: "query", Schema: &fusedobject.SchemaContract{}, Content: map[string]fusedobject.ParameterContent{"application/json": {}}}}},
		{name: "missing schema and content", parameters: fusedobject.Parameters{{Name: "q", In: "query"}}},
		{name: "wrong style", parameters: fusedobject.Parameters{{Name: "X-Test", In: "header", Schema: schema, Serialization: fusedobject.ParameterSerialization{Style: "deepObject"}}}},
		{name: "multiple content", parameters: fusedobject.Parameters{{Name: "q", In: "query", Serialization: fusedobject.ParameterSerialization{Explode: &falseValue}, Content: map[string]fusedobject.ParameterContent{"application/json": {}, "text/plain": {}}}}},
		{name: "querystring media", parameters: fusedobject.Parameters{{Name: "q", In: "querystring", Content: map[string]fusedobject.ParameterContent{"application/xml": {}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateEndpointTransport(fusedobject.Endpoint{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, Parameters: test.parameters}); err == nil {
				t.Fatal("expected invalid parameter contract")
			}
		})
	}
}

func TestValidateEndpointTransportRequiresExactPathDeclarations(t *testing.T) {
	schema := operationTestSchemaContract()
	for name, endpoint := range map[string]fusedobject.Endpoint{
		"undeclared placeholder": {Method: "GET", Path: "/items/{id}", ProviderProtocol: models.ProviderProtocolREST},
		"unused declaration":     {Method: "GET", Path: "/items", ProviderProtocol: models.ProviderProtocolREST, Parameters: fusedobject.Parameters{{Name: "id", In: "path", Required: true, Schema: schema}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateEndpointTransport(endpoint); err == nil {
				t.Fatal("expected path declaration rejection")
			}
		})
	}
	valid := fusedobject.Endpoint{Method: "GET", Path: "/items/{id}", ProviderProtocol: models.ProviderProtocolREST, Parameters: fusedobject.Parameters{{Name: "id", In: "path", Required: true, Schema: schema}}}
	if err := validateEndpointTransport(valid); err != nil {
		t.Fatalf("exact path declaration rejected: %v", err)
	}
}

func TestValidateEndpointTransportAcceptsOpenAPI32ParameterSerialization(t *testing.T) {
	allowReserved := true
	endpoint := fusedobject.Endpoint{Method: "QUERY", Parameters: fusedobject.Parameters{
		{Name: "selector", In: "querystring", Content: map[string]fusedobject.ParameterContent{"application/json": {}}},
		{Name: "X-Selector", In: "header", Schema: operationTestSchemaContract(), Serialization: fusedobject.ParameterSerialization{AllowReserved: &allowReserved}},
	}}
	endpoint.ProviderProtocol = models.ProviderProtocolREST
	// Querystring cannot coexist with ordinary query parameters, but header
	// reserved expansion is pass-through and remains protected by CRLF checks.
	if err := validateEndpointTransport(endpoint); err != nil {
		t.Fatalf("OpenAPI 3.2 parameter contract rejected: %v", err)
	}
}

func TestValidateEndpointTransportAcceptsSupportedQuerystringMedia(t *testing.T) {
	for _, mediaType := range []string{
		"application/x-www-form-urlencoded; charset=utf-8", "application/json", "application/jsonpath", "text/plain; charset=utf-8",
	} {
		endpoint := fusedobject.Endpoint{Method: "QUERY", ProviderProtocol: models.ProviderProtocolREST, Parameters: fusedobject.Parameters{{
			Name: "selector", In: "querystring", Content: map[string]fusedobject.ParameterContent{mediaType: {}},
		}}}
		if err := validateEndpointTransport(endpoint); err != nil {
			t.Fatalf("accepted querystring media %q: %v", mediaType, err)
		}
	}
}

func TestValidateEndpointTransportRejectsDuplicateServers(t *testing.T) {
	server := fusedobject.Server{URL: "https://api.example.com", Environment: "production"}
	endpoint := fusedobject.Endpoint{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, OperationServers: fusedobject.Servers{server, server}}
	if err := validateEndpointTransport(endpoint); err == nil {
		t.Fatal("expected duplicate operation server rejection")
	}
}

func TestValidateEndpointTransportRejectsDuplicateOpenAPI32ServerNames(t *testing.T) {
	endpoint := fusedobject.Endpoint{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, OperationServers: fusedobject.Servers{
		{URL: "https://one.example.com", Name: "Production"},
		{URL: "https://two.example.com", Name: "production"},
	}}
	if err := validateEndpointTransport(endpoint); err == nil {
		t.Fatal("expected duplicate OpenAPI 3.2 server name rejection")
	}
}

func boolPointer(value bool) *bool { return &value }

func TestValidateEndpointTransportRequiresExplicitProviderProtocol(t *testing.T) {
	query := "query Viewer { viewer { id } }"
	tests := []fusedobject.Endpoint{
		{Method: "GET"},
		{Method: "POST", GraphQLQuery: &query, OperationKind: models.OperationKindQuery},
		{Method: "POST", ProviderProtocol: models.ProviderProtocolGraphQL, GraphQLQuery: &query},
		{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, GraphQLQuery: &query},
	}
	for _, endpoint := range tests {
		if err := validateEndpointTransport(endpoint); err == nil {
			t.Fatalf("incomplete protocol contract accepted: %#v", endpoint)
		}
	}
	valid := fusedobject.Endpoint{
		Method: "POST", ProviderProtocol: models.ProviderProtocolGraphQL,
		GraphQLQuery: &query, OperationKind: models.OperationKindQuery,
	}
	if err := validateEndpointTransport(valid); err != nil {
		t.Fatalf("explicit GraphQL contract rejected: %v", err)
	}
}

func operationTestSchemaContract() *fusedobject.SchemaContract {
	raw := []byte(`{"type":"string"}`)
	hash, err := canonicaljson.SHA256(raw)
	if err != nil {
		panic(err)
	}
	return &fusedobject.SchemaContract{
		Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw,
		ContentHash: hex.EncodeToString(hash[:]), Projection: fusedobject.Schema{Type: "string"},
	}
}
