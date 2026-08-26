package sandbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestMeasureMCPSchemaEncodedByteLimit locks the inclusive per-schema byte
// ceiling used by both offline fixtures and live session admission.
func TestMeasureMCPSchemaEncodedByteLimit(t *testing.T) {
	atLimit := json.RawMessage(`"` + strings.Repeat("a", maxMCPSchemaEncodedBytes-2) + `"`)
	// Inclusive limits allow an exactly bounded schema to cross admission.
	if _, err := measureMCPSchema(atLimit); err != nil {
		t.Fatalf("measureMCPSchema(at limit) error = %v", err)
	}
	overLimit := json.RawMessage(`"` + strings.Repeat("a", maxMCPSchemaEncodedBytes-1) + `"`)
	// One byte over the public ceiling must retain the stable typed classification.
	if _, err := measureMCPSchema(overLimit); !errors.Is(err, ErrMCPSchemaEncodedBytesLimit) {
		t.Fatalf("measureMCPSchema(over limit) error = %v, want %v", err, ErrMCPSchemaEncodedBytesLimit)
	}
}

// TestMeasureMCPSchemaDepthLimit proves iterative traversal accepts the exact
// nesting ceiling and rejects the next container without recursive walking.
func TestMeasureMCPSchemaDepthLimit(t *testing.T) {
	atLimit := mcpTestNestedArraySchema(maxMCPSchemaDepth)
	// Depth is inclusive so authored schemas can rely on the documented maximum.
	if _, err := measureMCPSchema(atLimit); err != nil {
		t.Fatalf("measureMCPSchema(at depth limit) error = %v", err)
	}
	// The first container beyond the hard ceiling is rejected before its child is read.
	if _, err := measureMCPSchema(mcpTestNestedArraySchema(maxMCPSchemaDepth + 1)); !errors.Is(err, ErrMCPSchemaDepthLimit) {
		t.Fatalf("measureMCPSchema(over depth limit) error = %v, want %v", err, ErrMCPSchemaDepthLimit)
	}
}

// TestMeasureMCPSchemaNodeLimit bounds wide schemas independently from their
// shallow depth and encoded-byte size.
func TestMeasureMCPSchemaNodeLimit(t *testing.T) {
	atLimit := mcpTestArraySchema(maxMCPSchemaNodes - 1)
	// The opening container and its children may consume the complete node budget.
	if nodes, err := measureMCPSchema(atLimit); err != nil || nodes != maxMCPSchemaNodes {
		t.Fatalf("measureMCPSchema(at node limit) = %d/%v, want %d/nil", nodes, err, maxMCPSchemaNodes)
	}
	// The array container plus max scalar children exceeds the independent node budget.
	if _, err := measureMCPSchema(mcpTestArraySchema(maxMCPSchemaNodes)); !errors.Is(err, ErrMCPSchemaNodeLimit) {
		t.Fatalf("measureMCPSchema(over node limit) error = %v, want %v", err, ErrMCPSchemaNodeLimit)
	}
}

// TestValidateMCPFixtureSchemasAggregateLimit ensures many individually safe
// contracts cannot accumulate into an unbounded MCP catalogue.
func TestValidateMCPFixtureSchemasAggregateLimit(t *testing.T) {
	parameters := make([]models.Parameter, 11)
	for index := range parameters {
		// Each raw schema plus its empty runtime projection charges exactly 10,000 nodes.
		parameters[index].Schema = &models.SchemaContract{Raw: mcpTestArraySchema(maxMCPSchemaNodes - 2)}
	}
	atLimit := &Fixture{Operations: []FixtureOperation{{Parameters: parameters[:10]}}}
	// Aggregate admission is inclusive across independently valid schemas.
	if err := validateMCPFixtureSchemas(atLimit); err != nil {
		t.Fatalf("validateMCPFixtureSchemas(at aggregate limit) error = %v", err)
	}
	overLimit := &Fixture{Operations: []FixtureOperation{{Parameters: parameters}}}
	// The eleventh safe schema must fail under the shared fixture budget.
	if err := validateMCPFixtureSchemas(overLimit); !errors.Is(err, ErrMCPSchemaAggregateNodesLimit) {
		t.Fatalf("validateMCPFixtureSchemas(over aggregate limit) error = %v, want %v", err, ErrMCPSchemaAggregateNodesLimit)
	}
}

// TestValidateMCPFixtureSchemasCoversNestedAndLogicalSchemas guards schema
// surfaces that do not appear as top-level physical request contracts.
func TestValidateMCPFixtureSchemasCoversNestedAndLogicalSchemas(t *testing.T) {
	tooDeep := mcpTestNestedArraySchema(maxMCPSchemaDepth + 1)
	tests := []struct {
		name    string
		fixture *Fixture
	}{
		{
			name: "nested encoding header",
			fixture: &Fixture{Operations: []FixtureOperation{{RequestContent: &models.RequestContent{Representations: []models.RequestRepresentation{{
				ItemEncoding: &models.RequestEncoding{Headers: map[string]models.HeaderContract{"X-Cursor": {Schema: &models.SchemaContract{Raw: tooDeep}}}},
			}}}}}},
		},
		{
			name: "Unified target output",
			fixture: &Fixture{UnifiedOperations: &models.SDKUnifiedOperationDescriptors{Operations: []models.SDKUnifiedOperationDescriptor{{
				InputSchema: json.RawMessage(`{"type":"object"}`), Targets: []models.SDKUnifiedTargetDescriptor{{OutputSchema: tooDeep}},
			}}}},
		},
	}
	// Both cases represent schema branches outside the ordinary request body path.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Every documented schema location shares the same depth classification.
			if err := validateMCPFixtureSchemas(test.fixture); !errors.Is(err, ErrMCPSchemaDepthLimit) {
				t.Fatalf("validateMCPFixtureSchemas() error = %v, want %v", err, ErrMCPSchemaDepthLimit)
			}
		})
	}
}

// TestValidateMCPFixtureSchemasRejectsCyclicProjection proves an in-memory
// schema cycle reaches the depth ceiling before a recursive encoder runs.
func TestValidateMCPFixtureSchemasRejectsCyclicProjection(t *testing.T) {
	projection := models.Schema{Type: "array"}
	projection.Items = &projection
	fixture := &Fixture{Operations: []FixtureOperation{{Parameters: []models.Parameter{{Schema: &models.SchemaContract{Projection: projection}}}}}}
	// The iterative depth boundary owns rejection, without relying on encoder cycle detection.
	if err := validateMCPFixtureSchemas(fixture); !errors.Is(err, ErrMCPSchemaDepthLimit) {
		t.Fatalf("validateMCPFixtureSchemas(cyclic projection) error = %v, want %v", err, ErrMCPSchemaDepthLimit)
	}
}

// TestValidateMCPFixtureSchemasBoundsCyclicEncodings proves iterative scanning cannot loop forever on in-memory graphs.
func TestValidateMCPFixtureSchemasBoundsCyclicEncodings(t *testing.T) {
	encodings := make(map[string]models.RequestEncoding)
	encodings["loop"] = models.RequestEncoding{Encoding: encodings}
	fixture := &Fixture{Operations: []FixtureOperation{{RequestContent: &models.RequestContent{Representations: []models.RequestRepresentation{{Encoding: encodings}}}}}}
	// The aggregate contract budget is the finite escape hatch for cyclic metadata.
	if err := validateMCPFixtureSchemas(fixture); !errors.Is(err, ErrMCPSchemaAggregateNodesLimit) {
		t.Fatalf("validateMCPFixtureSchemas(cyclic encoding) error = %v, want %v", err, ErrMCPSchemaAggregateNodesLimit)
	}
}

// TestMCPEncodingQueueBoundsBranchingCycle proves pending siblings reserve
// budget before a cyclic graph can multiply queued copies.
func TestMCPEncodingQueueBoundsBranchingCycle(t *testing.T) {
	encodings := make(map[string]models.RequestEncoding)
	encodings["left"] = models.RequestEncoding{Encoding: encodings}
	encodings["right"] = models.RequestEncoding{Encoding: encodings}
	admission := &mcpSchemaAdmission{}
	// The initial two branches fit; their repeated fanout must not outgrow the shared budget.
	if err := admission.enqueueEncodings(encodings, nil, nil); err != nil {
		t.Fatalf("enqueueEncodings() error = %v", err)
	}
	if err := admission.drainEncodings(); !errors.Is(err, ErrMCPSchemaAggregateNodesLimit) {
		t.Fatalf("drainEncodings(branching cycle) error = %v, want %v", err, ErrMCPSchemaAggregateNodesLimit)
	}
	// Count pending plus completed work, not only visits, to catch the former queue-growth bypass.
	if admission.aggregateNodes+len(admission.encodings) > maxMCPAggregateSchemaNodes {
		t.Fatalf("encoding work exceeded aggregate budget: visited=%d pending=%d", admission.aggregateNodes, len(admission.encodings))
	}
}

// TestMCPEncodingQueueRejectsPrefixBeforeCopy proves one wide prefix append
// cannot allocate queue entries beyond the remaining aggregate reservation.
func TestMCPEncodingQueueRejectsPrefixBeforeCopy(t *testing.T) {
	admission := &mcpSchemaAdmission{aggregateNodes: maxMCPAggregateSchemaNodes - 1}
	prefix := make([]models.RequestEncoding, 2)
	// The complete append must be rejected atomically when only one slot remains.
	if err := admission.enqueueEncodings(nil, prefix, nil); !errors.Is(err, ErrMCPSchemaAggregateNodesLimit) {
		t.Fatalf("enqueueEncodings(prefix) error = %v, want %v", err, ErrMCPSchemaAggregateNodesLimit)
	}
	if len(admission.encodings) != 0 {
		t.Fatalf("rejected prefix allocated %d pending entries", len(admission.encodings))
	}
}

// TestLoadFixtureRejectsOversizedSchemaWithoutEcho verifies the offline file
// boundary wraps the typed failure without copying authored content into it.
func TestLoadFixtureRejectsOversizedSchemaWithoutEcho(t *testing.T) {
	marker := "private-schema-marker"
	description := marker + strings.Repeat("x", maxMCPSchemaEncodedBytes)
	fixture := Fixture{UnifiedOperations: &models.SDKUnifiedOperationDescriptors{
		SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion,
		Operations:    []models.SDKUnifiedOperationDescriptor{{Name: "oversized", InputSchema: mcpTestDescriptionSchema(t, description)}},
	}}
	encoded, err := json.Marshal(fixture)
	// The test fixture itself must remain valid so admission owns the failure.
	if err != nil {
		t.Fatalf("json.Marshal(fixture) error = %v", err)
	}
	_, err = LoadFixture(writeTempFixture(t, string(encoded)))
	// Wrapped errors preserve machine classification while keeping raw schema text private.
	if !errors.Is(err, ErrMCPSchemaEncodedBytesLimit) {
		t.Fatalf("LoadFixture(oversized schema) error = %v, want %v", err, ErrMCPSchemaEncodedBytesLimit)
	}
	// Stable failures must not echo content that could contain proprietary schemas.
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("LoadFixture() error leaked schema marker: %v", err)
	}
}

// mcpTestNestedArraySchema creates a deterministic valid schema-shaped JSON
// value with the requested container depth.
func mcpTestNestedArraySchema(depth int) json.RawMessage {
	return json.RawMessage(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
}

// mcpTestArraySchema creates one shallow JSON array with a predictable token
// count: one container node plus scalarCount scalar nodes.
func mcpTestArraySchema(scalarCount int) json.RawMessage {
	return json.RawMessage("[" + strings.Repeat("0,", scalarCount-1) + "0]")
}

// mcpTestDescriptionSchema builds a JSON Schema document through the standard
// encoder so test data never depends on hand-authored escaping.
func mcpTestDescriptionSchema(t *testing.T, description string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"description": description, "type": "object"})
	// String-only schema fields must encode before admission can be exercised.
	if err != nil {
		t.Fatalf("json.Marshal(description schema) error = %v", err)
	}
	return encoded
}
