// Package bridgehost owns authenticated reverse sessions and immutable local-provider contracts.
package bridgehost

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/jsonschema-go/jsonschema"
)

// MaxPayload accommodates encoded MQTT payloads while bounding every durable envelope.
const MaxPayload = 1 << 20
const MaxManifest = 2 << 20

var ErrContract = errors.New("bridge contract invalid")
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

// ManifestDocument contains only credential-free selected operation contracts.
type ManifestDocument struct {
	Version    int         `json:"version"`
	State      string      `json:"state,omitempty"`
	ProfileID  string      `json:"profile_id"`
	Revision   string      `json:"revision"`
	Operations []Operation `json:"operations"`
}

// Operation retains exact contract bytes so both peers compute the same digest.
type Operation struct {
	ConnectionID string `json:"connection_id"`
	OperationID  string `json:"operation_id"`
	ContractHash string `json:"contract_hash"`
	ContractJSON string `json:"contract_json"`
}

// Contract deliberately excludes configuration, provider credentials and mutable routing.
type Contract struct {
	Kind         string          `json:"kind"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// Digest names exact immutable bytes rather than a language-specific reserialization.
func Digest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

// decode rejects undeclared metadata and trailing documents at the trust boundary.
func decode(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	// Invalid JSON cannot acquire an immutable contract identity.
	if err := d.Decode(target); err != nil {
		return ErrContract
	}
	var extra any
	// A valid first object must not conceal another document.
	if d.Decode(&extra) != io.EOF {
		return ErrContract
	}
	return nil
}

// ValidateManifest binds every operation and outer identity before storage or acknowledgement.
func ValidateManifest(frame *bridgev1.Manifest) (*ManifestDocument, error) {
	// Bound parsing cost and require the claimed hash to identify the received bytes.
	if frame == nil || len(frame.ManifestJson) > MaxManifest || Digest(frame.ManifestJson) != frame.Hash {
		return nil, ErrContract
	}
	var doc ManifestDocument
	// The frame and document must identify the same immutable revision.
	if decode(frame.ManifestJson, &doc) != nil || doc.Version != 1 || doc.ProfileID != frame.ProfileId || doc.Revision != frame.Revision {
		return nil, ErrContract
	}
	// Semantic bounds and uniqueness are checked separately from wire identity.
	if err := validateManifestDocument(doc); err != nil {
		return nil, err
	}

	return &doc, nil
}

// Contract verifies the claimed digest and limits the first transport version to unary/events.
func (op Operation) Contract() (*Contract, error) {
	// Operation identities cannot claim a schema different from the preserved bytes.
	if len(op.ContractJSON) > MaxPayload || Digest([]byte(op.ContractJSON)) != op.ContractHash {
		return nil, ErrContract
	}
	var c Contract
	// Streaming needs explicit framing and must not be accidentally admitted as unary.
	if decode([]byte(op.ContractJSON), &c) != nil || (c.Kind != "read" && c.Kind != "command" && c.Kind != "event") {
		return nil, ErrContract
	}
	for _, raw := range []json.RawMessage{c.InputSchema, c.OutputSchema} {
		var schema jsonschema.Schema
		// Resolve without remote loaders: a contract can never initiate schema URL fetches.
		if json.Unmarshal(raw, &schema) != nil {
			return nil, ErrContract
		}
		// Unresolved references cannot validate later calls safely.
		if _, err := schema.Resolve(nil); err != nil {
			return nil, ErrContract
		}
	}
	return &c, nil
}

// ValidatePayload preserves JSON root shapes and returns no sensitive validation details.
func ValidatePayload(rawSchema, payload []byte) error {
	// Bound payloads before schema work or memory allocation.
	if len(payload) > MaxPayload || !json.Valid(payload) {
		return ErrContract
	}
	var schema jsonschema.Schema
	// Only stored valid schemas should reach this path; corruption still fails closed.
	if json.Unmarshal(rawSchema, &schema) != nil {
		return ErrContract
	}
	resolved, err := schema.Resolve(nil)
	// Never execute against an unresolved contract.
	if err != nil {
		return ErrContract
	}
	var value any
	// JSON numbers use the same representation as the schema validator.
	if json.Unmarshal(payload, &value) != nil || resolved.Validate(value) != nil {
		return ErrContract
	}
	return nil
}

// validateManifestDocument permits capability withdrawal while bounding catalogue size and identities.
func validateManifestDocument(doc ManifestDocument) error {
	// Empty catalogues withdraw current capabilities; bounds still limit admission cost.
	if !identifier.MatchString(doc.ProfileID) || !identifier.MatchString(doc.Revision) || len(doc.Operations) > 1000 {
		return ErrContract
	}
	// Lifecycle declarations cannot expose operations for a removed profile.
	if err := validateManifestState(doc); err != nil {
		return err
	}
	return validateManifestOperations(doc.Operations)
}

// validateManifestOperations rejects ambiguous identities before accepting any schema.
func validateManifestOperations(operations []Operation) error {
	seen := map[string]bool{}
	for _, op := range operations {
		key := op.ConnectionID + "\x00" + op.OperationID
		// Duplicate identities would make exact app selection ambiguous.
		if seen[key] || !identifier.MatchString(op.ConnectionID) || !identifier.MatchString(op.OperationID) {
			return ErrContract
		}
		seen[key] = true
		// Every nested contract is validated before any manifest is accepted.
		if _, err := op.Contract(); err != nil {
			return err
		}
	}
	return nil
}

// manifestState preserves older immutable manifest bytes with their original online semantics.
func manifestState(doc ManifestDocument) string {
	// Absent state belongs to manifests produced before workspace inventory support.
	if doc.State == "" {
		return "online"
	}
	return doc.State
}

// validateManifestState keeps the lifecycle vocabulary finite and withdrawal unambiguous.
func validateManifestState(doc ManifestDocument) error {
	state := manifestState(doc)
	// Unknown states must not accidentally acquire live execution authority.
	if state != "online" && state != "offline" && state != "removed" {
		return ErrContract
	}
	// Removed profiles cannot advertise callable operations.
	if state == "removed" && len(doc.Operations) > 0 {
		return ErrContract
	}
	return nil
}
