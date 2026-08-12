package sandbox

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

func TestValidateEndpointMediaContractsPreservesSchemaAndStatusRanges(t *testing.T) {
	schema := testSchemaContract([]byte(`{"oneOf":[{"type":"string"},{"type":"null"}]}`))
	endpoint := fusedobject.Endpoint{
		Method: "POST", ProviderProtocol: models.ProviderProtocolREST,
		RequestContent: &fusedobject.RequestContent{
			DefaultMediaType: "application/vnd.test+json",
			Representations: []fusedobject.RequestRepresentation{
				{MediaType: "application/vnd.test+json", Serialization: fusedobject.RequestSerializationJSON, Schema: schema},
				{MediaType: "application/xml", Serialization: fusedobject.RequestSerializationRaw},
			},
		},
		Responses: fusedobject.Responses{"2XX": {
			Description:     "success",
			Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/vnd.test+json", Schema: schema}},
		}},
	}
	if err := validateEndpointTransport(endpoint); err != nil {
		t.Fatalf("validateEndpointTransport: %v", err)
	}
}

func TestValidateEndpointMediaContractsRejectsHashAndAmbiguousMedia(t *testing.T) {
	schema := testSchemaContract([]byte(`{"type":"object"}`))
	schema.ContentHash = "00"
	endpoint := fusedobject.Endpoint{Method: "POST", ProviderProtocol: models.ProviderProtocolREST, RequestContent: &fusedobject.RequestContent{
		Representations: []fusedobject.RequestRepresentation{
			{MediaType: "application/json", Serialization: fusedobject.RequestSerializationJSON, Schema: schema},
			{MediaType: "application/xml", Serialization: fusedobject.RequestSerializationRaw},
		},
	}}
	if err := validateEndpointTransport(endpoint); err == nil {
		t.Fatal("expected ambiguous media/hash rejection")
	}
	endpoint.RequestContent.DefaultMediaType = "application/json"
	if err := validateEndpointTransport(endpoint); err == nil {
		t.Fatal("expected schema hash rejection")
	}
}

func TestValidateEndpointMediaContractsRejectsMissingRepresentationsAndCanonicalInference(t *testing.T) {
	direct := fusedobject.Endpoint{
		Method: "POST", ProviderProtocol: models.ProviderProtocolREST,
		RequestContent: &fusedobject.RequestContent{},
	}
	if err := validateEndpointTransport(direct); err == nil {
		t.Fatal("request content without representations was accepted")
	}
	canonical := direct
	canonical.RequestContent = &fusedobject.RequestContent{Representations: []fusedobject.RequestRepresentation{{MediaType: "application/json"}}}
	if err := validateEndpointTransport(canonical); err == nil {
		t.Fatalf("canonical serialization inference accepted: %#v", canonical.RequestContent)
	}
}

func TestValidateSchemaContractUsesSemanticCanonicalHash(t *testing.T) {
	hash, err := canonicaljson.SHA256([]byte(`{"required":["name"],"type":"object","properties":{"name":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("canonical hash: %v", err)
	}
	contract := &fusedobject.SchemaContract{
		Dialect:     "https://json-schema.org/draft/2020-12/schema",
		Raw:         []byte(` { "properties": { "name": { "type": "string" } }, "type": "object", "required": ["name"] } `),
		ContentHash: hex.EncodeToString(hash[:]),
	}
	if err := validateSchemaContract(contract); err != nil {
		t.Fatalf("validateSchemaContract rejected semantic hash: %v", err)
	}
	contract.Raw = []byte(`{"type":"array"}`)
	if err := validateSchemaContract(contract); err == nil {
		t.Fatal("validateSchemaContract accepted a different schema")
	}
}

func TestValidateSchemaContractAcceptsBooleanRoot(t *testing.T) {
	contract := testSchemaContract([]byte(`true`))
	if err := validateSchemaContract(contract); err != nil {
		t.Fatalf("validateSchemaContract rejected boolean schema: %v", err)
	}
}

func TestValidateSchemaContractRejectsNonCanonicalHashCase(t *testing.T) {
	contract := testSchemaContract([]byte(`{"type":"object"}`))
	contract.ContentHash = strings.ToUpper(contract.ContentHash)
	if err := validateSchemaContract(contract); err == nil {
		t.Fatal("validateSchemaContract accepted a non-lowercase content hash")
	}
}

func TestValidateEndpointRejectsInvalidSchemaInNestedEncodingHeader(t *testing.T) {
	invalid := testSchemaContract([]byte(`{"type":"object"}`))
	invalid.ContentHash = strings.Repeat("0", 64)
	nested := fusedobject.ParameterContent{Encoding: map[string]fusedobject.RequestEncoding{
		"payload": {Headers: map[string]fusedobject.HeaderContract{"X-Nested": {Schema: invalid}}},
	}}
	tests := []fusedobject.Endpoint{
		{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, Parameters: fusedobject.Parameters{{Name: "filter", In: "query", Content: map[string]fusedobject.ParameterContent{"application/json": nested}}}},
		{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, Responses: fusedobject.Responses{"200": {Headers: map[string]fusedobject.HeaderContract{"X-Outer": {Content: map[string]fusedobject.ParameterContent{"application/json": nested}}}}}},
	}
	for _, endpoint := range tests {
		if err := validateEndpointTransport(endpoint); err == nil {
			t.Fatal("validateEndpointTransport accepted a nested invalid schema hash")
		}
	}
}

func TestValidateEndpointMediaContractsAcceptsExecutedOpenAPI32Streams(t *testing.T) {
	itemSchema := testSchemaContract([]byte(`{"type":"object"}`))
	itemSchema.Projection.Type = "object"
	tests := []fusedobject.RequestRepresentation{
		{MediaType: "application/x-ndjson", Serialization: fusedobject.RequestSerializationRaw, ItemSchema: itemSchema},
		{MediaType: "multipart/mixed", Serialization: fusedobject.RequestSerializationMultipart, ItemSchema: itemSchema,
			PrefixEncoding: []fusedobject.RequestEncoding{{ContentType: "application/json"}}, ItemEncoding: &fusedobject.RequestEncoding{ContentType: "application/octet-stream"}},
	}
	for _, representation := range tests {
		endpoint := fusedobject.Endpoint{Method: "POST", ProviderProtocol: models.ProviderProtocolREST, RequestContent: &fusedobject.RequestContent{
			PayloadParameter: "body", Representations: []fusedobject.RequestRepresentation{representation},
		}}
		if err := validateEndpointTransport(endpoint); err != nil {
			t.Fatalf("media %s rejected: %v", representation.MediaType, err)
		}
	}
}

func TestValidateEndpointMediaContractsRejectsUnsupportedItemDirections(t *testing.T) {
	itemSchema := testSchemaContract([]byte(`{"type":"object"}`))
	tests := []fusedobject.RequestRepresentation{
		{MediaType: "application/json", ItemSchema: itemSchema},
		{MediaType: "text/event-stream", Serialization: fusedobject.RequestSerializationRaw, ItemSchema: itemSchema},
		{MediaType: "multipart/mixed", Serialization: fusedobject.RequestSerializationMultipart, ItemSchema: itemSchema,
			ItemEncoding: &fusedobject.RequestEncoding{ContentType: "multipart/mixed", ItemEncoding: &fusedobject.RequestEncoding{
				ContentType: "multipart/mixed", ItemEncoding: &fusedobject.RequestEncoding{ContentType: "application/json"},
			}}},
	}
	for _, representation := range tests {
		endpoint := fusedobject.Endpoint{Method: "POST", ProviderProtocol: models.ProviderProtocolREST, RequestContent: &fusedobject.RequestContent{
			PayloadParameter: "body", Representations: []fusedobject.RequestRepresentation{representation},
		}}
		if err := validateEndpointTransport(endpoint); err == nil {
			t.Fatalf("unsupported media contract accepted: %#v", representation)
		}
	}
}

func TestValidateEndpointMediaContractsRejectsNestedNamedEncoding(t *testing.T) {
	representation := fusedobject.RequestRepresentation{MediaType: "multipart/form-data", Serialization: fusedobject.RequestSerializationMultipart,
		Encoding: map[string]fusedobject.RequestEncoding{"payload": {
			ContentType: "multipart/mixed", PrefixEncoding: []fusedobject.RequestEncoding{{ContentType: "text/plain"}},
		}},
	}
	endpoint := fusedobject.Endpoint{Method: "POST", ProviderProtocol: models.ProviderProtocolREST, RequestContent: &fusedobject.RequestContent{
		PayloadParameter: "body", Representations: []fusedobject.RequestRepresentation{representation},
	}}
	if err := validateEndpointTransport(endpoint); err == nil {
		t.Fatal("nested named encoding accepted without a property-aware writer")
	}
}

func TestValidateResponseItemSchemaRequiresSequentialMedia(t *testing.T) {
	itemSchema := testSchemaContract([]byte(`{"type":"object"}`))
	valid := fusedobject.Endpoint{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, Responses: fusedobject.Responses{"200": {
		Representations: []fusedobject.ResponseRepresentation{{MediaType: "text/event-stream", ItemSchema: itemSchema, SSE: &fusedobject.SSEResponseContract{ItemMode: "data"}}},
	}}}
	if err := validateEndpointTransport(valid); err != nil {
		t.Fatalf("SSE response rejected: %v", err)
	}
	for _, mediaType := range []string{"application/json", "application/json-seq", "multipart/mixed"} {
		invalid := valid
		invalid.Responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{MediaType: mediaType, ItemSchema: itemSchema}}}}
		if err := validateEndpointTransport(invalid); err == nil {
			t.Fatalf("unframed response item schema accepted for %s", mediaType)
		}
	}
	for name, contract := range map[string]*fusedobject.SSEResponseContract{
		"mode": {ItemMode: "event"}, "empty sentinel": {ItemMode: "data", DoneSentinel: testStringPointer("")},
		"multiline sentinel": {ItemMode: "data", DoneSentinel: testStringPointer("done\nnext")},
	} {
		invalid := valid
		invalid.Responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{MediaType: "text/event-stream", ItemSchema: itemSchema, SSE: contract}}}}
		if err := validateEndpointTransport(invalid); err == nil {
			t.Fatalf("invalid SSE %s accepted", name)
		}
	}
}

func testStringPointer(value string) *string { return &value }

func testSchemaContract(raw []byte) *fusedobject.SchemaContract {
	hash, err := canonicaljson.SHA256(raw)
	if err != nil {
		panic(err)
	}
	return &fusedobject.SchemaContract{
		Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw,
		ContentHash: hex.EncodeToString(hash[:]), Projection: fusedobject.Schema{Type: "string"},
	}
}
