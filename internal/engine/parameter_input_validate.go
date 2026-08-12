package engine

import (
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/shared/models"
)

const customHeadersParameter = "_headers"

// ValidateDeclaredExecutionParameters rejects caller input the reviewed
// operation cannot place on the wire. Body fields are declared by the selected
// representation rather than being guessed from the HTTP method.
func ValidateDeclaredExecutionParameters(declared models.Parameters, content *SelectedRequestRepresentation, params map[string]any) error {
	parameterNames := make(map[string]struct{}, len(declared))
	for _, parameter := range declared {
		parameterNames[parameter.Name] = struct{}{}
	}
	for name, value := range params {
		if _, ok := parameterNames[name]; ok || declaredRequestBodyParameter(content, name) {
			continue
		}
		if name == customHeadersParameter {
			if _, ok := value.(map[string]any); !ok {
				return errors.New("reserved _headers parameter must be an object")
			}
			continue
		}
		return fmt.Errorf("undeclared execution parameter %q", name)
	}
	return nil
}

func declaredRequestBodyParameter(content *SelectedRequestRepresentation, name string) bool {
	if content == nil {
		return false
	}
	if content.PayloadParameter != "" && name == content.PayloadParameter {
		return true
	}
	if _, ok := content.Encoding[name]; ok {
		return true
	}
	if _, ok := content.DeclaredBodyFields[name]; ok {
		return true
	}
	if content.AllowsAdditionalBodyFields {
		return true
	}
	if content.Schema == nil {
		return false
	}
	if _, ok := content.Schema.Properties[name]; ok {
		return true
	}
	return content.Schema.AdditionalProperties != nil
}
