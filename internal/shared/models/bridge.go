package models

import "encoding/json"

// BridgeReference selects one local capability from an exact accepted manifest.
type BridgeConnectionSelection struct {
	Operations json.RawMessage `json:"operations" yaml:"operations"`
}

// A grouped authored selection is resolved to legacy exact pins during plan.
type BridgeReference struct {
	Bridge       string                               `json:"bridge,omitempty" yaml:"bridge,omitempty"`
	Profile      string                               `json:"profile,omitempty" yaml:"profile,omitempty"`
	Connections  map[string]BridgeConnectionSelection `json:"connections,omitempty" yaml:"connections,omitempty"`
	PairingID    string                               `json:"pairing_id,omitempty" yaml:"pairing_id,omitempty"`
	ProfileID    string                               `json:"profile_id,omitempty" yaml:"profile_id,omitempty"`
	ManifestHash string                               `json:"manifest_hash,omitempty" yaml:"manifest_hash,omitempty"`
	ConnectionID string                               `json:"connection_id,omitempty" yaml:"connection_id,omitempty"`
	OperationID  string                               `json:"operation_id,omitempty" yaml:"operation_id,omitempty"`
}

// BridgeBinding preserves the reviewed contract inside an immutable app publication.
type BridgeBinding struct {
	Group       string `json:"group,omitempty"`
	WorkspaceID string `json:"workspace_id"`
	BridgeReference
	Alias        string          `json:"alias"`
	ContractHash string          `json:"contract_hash"`
	ContractJSON string          `json:"contract_json"`
	Kind         string          `json:"kind"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}
