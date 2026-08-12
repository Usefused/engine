package sandbox

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func widgetGetOperation() *FixtureOperation {
	return &FixtureOperation{
		OperationID: "test.getWidget",
		Method:      "GET",
		Path:        "/widgets/{id}",
		Parameters: []models.Parameter{
			{Name: "id", In: "path", Required: true, Type: "string"},
			{Name: "verbose", In: "query", Required: false, Type: "boolean"},
		},
	}
}

func widgetCreateOperation() *FixtureOperation {
	return &FixtureOperation{
		OperationID: "test.createWidget",
		Method:      "POST",
		Path:        "/widgets",
		RequestContent: &models.RequestContent{
			Required: true,
			Representations: []models.RequestRepresentation{{
				MediaType: "application/json", Serialization: models.RequestSerializationJSON,
				Schema: &models.SchemaContract{Projection: models.Schema{
					Type:     "object",
					Required: []string{"name"},
					Properties: map[string]models.Schema{
						"name":  {Type: "string"},
						"count": {Type: "integer"},
					},
				}},
			}},
		},
	}
}

func rawUploadOperation() *FixtureOperation {
	return &FixtureOperation{
		OperationID: "test.uploadWidget",
		RequestContent: &models.RequestContent{
			PayloadParameter: "body", Representations: []models.RequestRepresentation{{
				MediaType: "application/octet-stream", Serialization: models.RequestSerializationRaw,
				Schema: &models.SchemaContract{Projection: models.Schema{Type: "string"}},
			}},
		},
	}
}

func TestValidateCallParams_MissingRequiredParameterRejected(t *testing.T) {
	err := validateCallParams(widgetGetOperation(), map[string]any{})
	if err == nil {
		t.Fatal("validateCallParams() with missing required path param = nil error, want error")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error = %v, want it to name the missing parameter %q", err, "id")
	}
}

func TestValidateCallParams_AllRequiredParametersPresentPasses(t *testing.T) {
	err := validateCallParams(widgetGetOperation(), map[string]any{"id": "widget-1"})
	if err != nil {
		t.Errorf("validateCallParams() = %v, want nil", err)
	}
}

func TestValidateCallParams_OptionalParameterMayBeOmitted(t *testing.T) {
	// "verbose" is declared but not required -- its absence must not be an error.
	err := validateCallParams(widgetGetOperation(), map[string]any{"id": "widget-1"})
	if err != nil {
		t.Errorf("validateCallParams() with optional param omitted = %v, want nil", err)
	}
}

func TestValidateCallParams_RequestContentMissingRequiredFieldRejected(t *testing.T) {
	err := validateCallParams(widgetCreateOperation(), map[string]any{"count": 3})
	if err == nil {
		t.Fatal("validateCallParams() with missing required body field = nil error, want error")
	}
}

func TestValidateCallParams_RequestContentWithRequiredFieldPasses(t *testing.T) {
	err := validateCallParams(widgetCreateOperation(), map[string]any{"name": "widget", "count": 3})
	if err != nil {
		t.Errorf("validateCallParams() = %v, want nil", err)
	}
}

func TestValidateCallParams_RequestContentWrongTypeRejected(t *testing.T) {
	// "name" is declared as a string; a script hallucinating a numeric value
	// here is exactly the failure mode this validation exists to catch
	// before it becomes a malformed vendor request.
	err := validateCallParams(widgetCreateOperation(), map[string]any{"name": 42})
	if err == nil {
		t.Fatal("validateCallParams() with wrong-typed body field = nil error, want error")
	}
}

func TestValidateCallParamsUsesRegistrySchemaProjectionOnly(t *testing.T) {
	raw := []byte(`{"oneOf":[{"type":"string"},{"type":"null"}]}`)
	op := &FixtureOperation{
		OperationID: "projected", Method: "POST", Path: "/projected",
		RequestContent: &models.RequestContent{Representations: []models.RequestRepresentation{{
			MediaType: "application/json", Serialization: models.RequestSerializationJSON,
			Schema: &models.SchemaContract{Raw: raw, Projection: models.Schema{
				Type: "object", Required: []string{"name"}, Properties: map[string]models.Schema{"name": {Type: "string"}},
			}},
		}}},
	}
	if err := validateCallParams(op, map[string]any{"name": "ok"}); err != nil {
		t.Fatalf("projection should accept request: %v", err)
	}
	if err := validateCallParams(op, map[string]any{}); err == nil {
		t.Fatal("projection-required property was not enforced")
	}
}

func TestValidateCallParams_RequiredRequestContentRejectsEmptyBody(t *testing.T) {
	err := validateCallParams(widgetCreateOperation(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "required request body") {
		t.Fatalf("empty required request content error = %v", err)
	}
}

func TestValidateCallParams_NoRequestContentRejectsUndeclaredInput(t *testing.T) {
	err := validateCallParams(widgetGetOperation(), map[string]any{"id": "widget-1", "unrelated": "value"})
	if err == nil || !strings.Contains(err.Error(), `undeclared execution parameter "unrelated"`) {
		t.Errorf("validateCallParams() undeclared input error = %v", err)
	}
}

func TestValidateCallParams_RawValidatesConfiguredPayloadValue(t *testing.T) {
	err := validateCallParams(rawUploadOperation(), map[string]any{"body": "payload"})
	if err != nil {
		t.Fatalf("valid raw payload rejected: %v", err)
	}
}

func TestValidateCallParams_RawRequiresPayloadConvention(t *testing.T) {
	op := rawUploadOperation()
	op.RequestContent.PayloadParameter = ""
	err := validateCallParams(op, map[string]any{"body": "payload"})
	if err == nil || !strings.Contains(err.Error(), `undeclared execution parameter "body"`) {
		t.Fatalf("raw payload convention error = %v", err)
	}
}

func TestValidateCallParams_RawRejectsMissingOrInvalidPayload(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]any
		binary      string
		wantMessage string
	}{
		{name: "missing", params: map[string]any{}, wantMessage: `missing raw request payload parameter "body"`},
		{name: "ambiguous extras", params: map[string]any{"body": "value", "extra": "lossy"}, wantMessage: `undeclared execution parameter "extra"`},
		{name: "wrong type", params: map[string]any{"body": map[string]any{"no": "json"}}, wantMessage: "string or byte array"},
		{name: "invalid base64", params: map[string]any{"body": "not-base64"}, binary: "base64", wantMessage: "invalid base64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := rawUploadOperation()
			if tt.binary == models.RequestBinaryEncodingBase64 {
				op.RequestContent.Representations[0].Schema.Projection.Format = "binary"
			}
			err := validateCallParams(op, tt.params)
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("raw validation error = %v, want %q", err, tt.wantMessage)
			}
		})
	}
}

func TestModelSchemaToOpenAPI_NilSchemaBecomesPermissiveObject(t *testing.T) {
	schema, err := modelSchemaToOpenAPI(nil)
	if err != nil {
		t.Fatalf("modelSchemaToOpenAPI(nil) error = %v", err)
	}
	if err := schema.VisitJSON(map[string]any{"anything": "goes"}); err != nil {
		t.Errorf("permissive object schema rejected arbitrary object: %v", err)
	}
}

func TestModelSchemaToOpenAPI_NestedArrayItemsValidate(t *testing.T) {
	s := &models.Schema{
		Type: "object",
		Properties: map[string]models.Schema{
			"tags": {
				Type:  "array",
				Items: &models.Schema{Type: "string"},
			},
		},
	}
	schema, err := modelSchemaToOpenAPI(s)
	if err != nil {
		t.Fatalf("modelSchemaToOpenAPI() error = %v", err)
	}

	if err := schema.VisitJSON(map[string]any{"tags": []any{"a", "b"}}); err != nil {
		t.Errorf("valid nested array rejected: %v", err)
	}
	if err := schema.VisitJSON(map[string]any{"tags": []any{1, 2}}); err == nil {
		t.Error("array of wrong-typed items accepted, want rejected")
	}
}

func TestModelSchemaToOpenAPI_AdditionalPropertiesValuesValidate(t *testing.T) {
	schema, err := modelSchemaToOpenAPI(&models.Schema{
		Type: "object", AdditionalProperties: &models.Schema{Type: "string"},
	})
	if err != nil {
		t.Fatalf("modelSchemaToOpenAPI: %v", err)
	}
	if err := schema.VisitJSON(map[string]any{"first": "valid", "second": "also-valid"}); err != nil {
		t.Fatalf("valid map values rejected: %v", err)
	}
	if err := schema.VisitJSON(map[string]any{"first": 42}); err == nil {
		t.Fatal("invalid map value accepted")
	}
}
