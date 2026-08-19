package unified

import (
	"encoding/json"

	"github.com/google/uuid"
)

const DefinitionSchemaVersion = 2

// OperationDefinition is the Engine-private, immutable representation of one
// Unified Operation. Exact provider identities prevent runtime name lookup from
// widening the SDK version's reviewed operation set.
type OperationDefinition struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	InputSchema json.RawMessage     `json:"input_schema"`
	Bindings    []BindingDefinition `json:"bindings"`
	Output      *OutputDefinition   `json:"output,omitempty"`
}

type BindingDefinition struct {
	PublicTarget     string              `json:"public_target"`
	ServiceTarget    string              `json:"service_target"`
	OperationID      string              `json:"operation_id"`
	ServiceID        uuid.UUID           `json:"service_id"`
	ServiceVersionID uuid.UUID           `json:"service_version_id"`
	EndpointID       uuid.UUID           `json:"endpoint_id"`
	DependsOn        []string            `json:"depends_on,omitempty"`
	Input            *Program            `json:"input,omitempty"`
	Output           *OutputDefinition   `json:"output,omitempty"`
	Rollback         *RollbackDefinition `json:"rollback,omitempty"`
}

// RollbackDefinition is one exact compensation operation whose mapping may
// read only the successful response of its own binding.
type RollbackDefinition struct {
	OperationID      string    `json:"operation_id"`
	ServiceID        uuid.UUID `json:"service_id"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	EndpointID       uuid.UUID `json:"endpoint_id"`
	Input            *Program  `json:"input,omitempty"`
}

// OutputDefinition contains the compiled projection and its validation schema.
type OutputDefinition struct {
	Schema  json.RawMessage `json:"schema"`
	Mapping *Program        `json:"mapping"`
}
