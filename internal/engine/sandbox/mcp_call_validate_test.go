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
		RequestBody: &models.Schema{
			Type:     "object",
			Required: []string{"name"},
			Properties: map[string]models.Schema{
				"name":  {Type: "string"},
				"count": {Type: "integer"},
			},
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

func TestValidateCallParams_RequestBodyMissingRequiredFieldRejected(t *testing.T) {
	err := validateCallParams(widgetCreateOperation(), map[string]any{"count": 3})
	if err == nil {
		t.Fatal("validateCallParams() with missing required body field = nil error, want error")
	}
}

func TestValidateCallParams_RequestBodyWithRequiredFieldPasses(t *testing.T) {
	err := validateCallParams(widgetCreateOperation(), map[string]any{"name": "widget", "count": 3})
	if err != nil {
		t.Errorf("validateCallParams() = %v, want nil", err)
	}
}

func TestValidateCallParams_RequestBodyWrongTypeRejected(t *testing.T) {
	// "name" is declared as a string; a script hallucinating a numeric value
	// here is exactly the failure mode this validation exists to catch
	// before it becomes a malformed vendor request.
	err := validateCallParams(widgetCreateOperation(), map[string]any{"name": 42})
	if err == nil {
		t.Fatal("validateCallParams() with wrong-typed body field = nil error, want error")
	}
}

func TestValidateCallParams_NoRequestBodyDeclaredSkipsBodyValidation(t *testing.T) {
	// widgetGetOperation has no RequestBody at all -- extra keys beyond the
	// declared path/query parameters are simply not something this operation
	// has a schema to validate them against.
	err := validateCallParams(widgetGetOperation(), map[string]any{"id": "widget-1", "unrelated": "value"})
	if err != nil {
		t.Errorf("validateCallParams() with no RequestBody declared = %v, want nil", err)
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
