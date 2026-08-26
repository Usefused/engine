package sandbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestMeasureMCPProjectionMatchesJSONNodes protects the reflection preflight
// from drifting away from the existing model's actual JSON representation.
func TestMeasureMCPProjectionMatchesJSONNodes(t *testing.T) {
	projection := models.Schema{
		Ref: "#/components/schemas/Widget", Type: "object", Format: "test",
		Properties: map[string]models.Schema{"id": {Type: "string"}},
		Items:      &models.Schema{Type: "string"}, AdditionalProperties: &models.Schema{Type: "number"}, Required: []string{"id"},
		Example: map[string]any{
			"nil": nil, "flag": true, "integer": 42, "large_number": json.Number("1e9999"),
			"escaped": "<tag>\n", "array": []any{"value", false}, "bytes": []byte("binary"),
			"raw": json.RawMessage(` {"nested":"<tag>"} `),
		},
	}
	encoded, err := json.Marshal(projection)
	// The reference encoding is small and valid, so it can safely establish parity.
	if err != nil {
		t.Fatalf("json.Marshal(projection) error = %v", err)
	}
	wantNodes, err := measureMCPSchema(encoded)
	if err != nil {
		t.Fatalf("measureMCPSchema(encoded) error = %v", err)
	}
	nodes, err := measureMCPProjection(projection)
	// Both scanners charge container, field-name, and scalar tokens identically.
	if err != nil || nodes != wantNodes {
		t.Fatalf("measureMCPProjection() = %d/%v, want %d/nil", nodes, err, wantNodes)
	}
}

// TestMeasureMCPProjectionEncodedByteBoundary keeps the pre-marshal byte
// ceiling inclusive even after object punctuation and string quoting.
func TestMeasureMCPProjectionEncodedByteBoundary(t *testing.T) {
	projection := models.Schema{Example: strings.Repeat("x", maxMCPSchemaEncodedBytes-len(`{"example":""}`))}
	// The full encoded object, not just its string leaf, may exactly consume the ceiling.
	if _, err := measureMCPProjection(projection); err != nil {
		t.Fatalf("measureMCPProjection(at byte limit) error = %v", err)
	}
	projection.Example = projection.Example.(string) + "x"
	if _, err := measureMCPProjection(projection); !errors.Is(err, ErrMCPSchemaEncodedBytesLimit) {
		t.Fatalf("measureMCPProjection(over byte limit) error = %v, want %v", err, ErrMCPSchemaEncodedBytesLimit)
	}
}

// TestMeasureMCPProjectionRejectsHugeShapes covers large leaves, width, and
// depth before recursive whole-document serialization is permitted.
func TestMeasureMCPProjectionRejectsHugeShapes(t *testing.T) {
	deep := models.Schema{}
	cursor := &deep
	for range maxMCPSchemaDepth * 100 {
		// A long acyclic chain must stop after the fixed depth budget, not after full traversal.
		cursor.Items = &models.Schema{}
		cursor = cursor.Items
	}
	tests := []struct {
		name       string
		projection models.Schema
		want       error
	}{
		{name: "large example string", projection: models.Schema{Example: strings.Repeat("x", maxMCPSchemaEncodedBytes*8)}, want: ErrMCPSchemaEncodedBytesLimit},
		{name: "wide example array", projection: models.Schema{Example: make([]any, maxMCPSchemaNodes+1)}, want: ErrMCPSchemaNodeLimit},
		{name: "deep projection", projection: deep, want: ErrMCPSchemaDepthLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Each resource dimension retains its existing stable failure code.
			if _, err := measureMCPProjection(test.projection); !errors.Is(err, test.want) {
				t.Fatalf("measureMCPProjection() error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestMeasureMCPProjectionRejectsCustomEncoder ensures hostile Example
// marshalers cannot run before the preflight budget check.
func TestMeasureMCPProjectionRejectsCustomEncoder(t *testing.T) {
	projection := models.Schema{Example: mcpPanicExampleEncoder{}}
	// A panic in the custom marshaler would expose any accidental whole-document encode.
	if _, err := measureMCPProjection(projection); !errors.Is(err, ErrMCPSchemaInvalid) {
		t.Fatalf("measureMCPProjection(custom encoder) error = %v, want %v", err, ErrMCPSchemaInvalid)
	}
}

// mcpPanicExampleEncoder detects an accidental call into an untrusted encoder.
type mcpPanicExampleEncoder struct{}

// MarshalJSON must never run because custom Example encoders are outside the
// JSON-native admission contract.
func (mcpPanicExampleEncoder) MarshalJSON() ([]byte, error) {
	panic("custom schema example encoder executed")
}
