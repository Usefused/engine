package models

import (
	"encoding/json"
	"github.com/google/uuid"
)

// UnifiedAppReference names an immutable hosted dependency without exposing runtime credentials.
type UnifiedAppReference struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
}

// UnifiedAppBinding freezes the public contract and exact runtime identity admitted by a plan.
type UnifiedAppBinding struct {
	Alias        string          `json:"alias"`
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	AppID        uuid.UUID       `json:"app_id"`
	AppFamilyID  uuid.UUID       `json:"app_family_id"`
	SourceHash   string          `json:"source_hash"`
	BundleDigest string          `json:"bundle_digest"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}
