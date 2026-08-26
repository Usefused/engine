package schemaref

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// TestLimitErrorsRemainDistinctFromBrokenReferences keeps public recovery honest without changing generic rejection checks.
func TestLimitErrorsRemainDistinctFromBrokenReferences(t *testing.T) {
	definitions := make(map[string]json.RawMessage, MaxDefinitions+1)
	for i := 0; i <= MaxDefinitions; i++ {
		definitions[fmt.Sprint(i)] = json.RawMessage("true")
	}
	_, err := New(definitions)
	// Budget failures retain the older invalidity category while adding an exact resource signal.
	if !errors.Is(err, ErrLimit) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("budget error=%v", err)
	}
	_, err = New(map[string]json.RawMessage{"Root": json.RawMessage(`{"$ref":"#/$defs/Missing"}`)})
	// A missing definition is a contract defect; asking users to shrink it would be misleading.
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrLimit) {
		t.Fatalf("reference error=%v", err)
	}
}
