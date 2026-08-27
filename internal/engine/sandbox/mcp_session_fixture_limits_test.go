package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// TestEndpointToFixtureOperationRejectsSchemaBeforeConversion guards the live pre-marshal boundary.
func TestEndpointToFixtureOperationRejectsSchemaBeforeConversion(t *testing.T) {
	endpoint := fusedobject.Endpoint{
		Name: "test.operation",
		Parameters: fusedobject.Parameters{{Name: "input", Schema: &fusedobject.SchemaContract{
			Projection: fusedobject.Schema{Example: strings.Repeat("secret", maxMCPSchemaEncodedBytes)},
		}}},
	}
	_, err := endpointToFixtureOperation("test-service", endpoint, nil)
	// The source representation must yield the same typed limit as an already-converted fixture.
	if !errors.Is(err, ErrMCPSchemaEncodedBytesLimit) {
		t.Fatalf("source schema admission = %v, want encoded-byte limit", err)
	}
	// Authored schema bytes may not leak through startup conversion errors.
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("source schema admission leaked rejected content")
	}
}

// TestEndpointToFixtureOperationBoundsSourceEncodingCycle avoids recursive JSON serialization before admission.
func TestEndpointToFixtureOperationBoundsSourceEncodingCycle(t *testing.T) {
	encoding := &fusedobject.RequestEncoding{}
	encoding.ItemEncoding = encoding
	endpoint := fusedobject.Endpoint{RequestContent: &fusedobject.RequestContent{
		Representations: []fusedobject.RequestRepresentation{{ItemEncoding: encoding}},
	}}
	_, err := endpointToFixtureOperation("test-service", endpoint, nil)
	// Cyclic source encodings must consume a finite work budget rather than reaching MarshalJSON recursion.
	if !errors.Is(err, ErrMCPSchemaAggregateNodesLimit) {
		t.Fatalf("source encoding cycle = %v, want aggregate work limit", err)
	}
}
