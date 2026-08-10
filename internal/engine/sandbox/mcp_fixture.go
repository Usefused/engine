package sandbox

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Usefused/engine/internal/shared/models"
)

// FixtureOperation is the spike's stand-in for a registered IntegrationObject,
// scoped to exactly what search_docs and call() need to resolve, route params
// for, and validate a call against one operation (sprint/lighter_mcp_runtime_spike_plan.md,
// Task 2). It intentionally reuses models.Parameter/models.Schema/models.Responses
// rather than inventing parallel types, so a real IntegrationObject can be
// mapped onto this shape later without a schema rewrite -- the fixture is a
// stand-in for a data *source*, not a different data *shape*.
type FixtureOperation struct {
	OperationID    string                 `json:"operation_id"`
	ServiceID      string                 `json:"service_id"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	Method         string                 `json:"method"`
	Path           string                 `json:"path"`
	Parameters     []models.Parameter     `json:"parameters"`
	RequestContent *models.RequestContent `json:"request_content,omitempty"`
	Responses      models.Responses       `json:"responses"`
}

// Fixture is the top-level shape of fixture.json.
type Fixture struct {
	Operations []FixtureOperation `json:"operations"`

	// byOperationID is built once at load time so Resolve is an O(1) map
	// lookup rather than a linear scan on every call() -- matters once this
	// stands in for a real catalog with more than a handful of operations.
	byOperationID map[string]*FixtureOperation
}

// LoadFixture reads and parses fixture.json from path, indexing operations by
// OperationID. This is the single load path used by both search_docs (Task 3,
// Node side, reading the same file) and call()'s Go-side resolution (Task 5/6)
// -- one source of truth for what an operationId means, per the design doc.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}

	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse fixture: %w", err)
	}

	f.byOperationID = make(map[string]*FixtureOperation, len(f.Operations))
	for i := range f.Operations {
		op := &f.Operations[i]
		if op.OperationID == "" {
			return nil, fmt.Errorf("fixture operation at index %d has no operation_id", i)
		}
		if _, exists := f.byOperationID[op.OperationID]; exists {
			return nil, fmt.Errorf("duplicate operation_id %q in fixture", op.OperationID)
		}
		f.byOperationID[op.OperationID] = op
	}

	return &f, nil
}

// Resolve looks up an operation by operationId. Returns false if the ID isn't
// in this MCP server's registered set -- this is the mechanical enforcement
// point for Trust and Governance Model tier 1 (design doc): an operationId
// outside the set simply fails to resolve, there's no downstream path that
// could act on an ID that was never registered.
func (f *Fixture) Resolve(operationID string) (*FixtureOperation, bool) {
	if f == nil {
		return nil, false
	}
	op, ok := f.byOperationID[operationID]
	return op, ok
}
