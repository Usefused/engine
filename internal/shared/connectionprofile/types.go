// Package connectionprofile owns the provider-neutral connection profile
// contract shared by every Registry and Engine ingress.
package connectionprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const ResourceDiscoveryVersion = 1

// Profile is the canonical, versioned provider behavior attached to a bucket.
// Publication provenance and visibility intentionally live outside this
// caller-controlled configuration object.
type Profile struct {
	AuthType          string                   `json:"auth_type,omitempty" yaml:"auth_type,omitempty"`
	AuthName          string                   `json:"auth_name,omitempty" yaml:"auth_name,omitempty"`
	OAuth2Flow        string                   `json:"oauth2_flow,omitempty" yaml:"oauth2_flow,omitempty"`
	ResourceDiscovery *ResourceDiscoveryConfig `json:"resource_discovery,omitempty" yaml:"resource_discovery,omitempty"`
	ResourceInput     *ResourceInputConfig     `json:"resource_input,omitempty" yaml:"resource_input,omitempty"`
	Metadata          map[string]string        `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Bindings          []Binding                `json:"bindings,omitempty" yaml:"bindings,omitempty"`
}

// UnmarshalJSON rejects publication controls before decoding because profile
// authors must never be able to grant their own provenance or visibility.
func (p *Profile) UnmarshalJSON(data []byte) error {
	type profileAlias Profile
	var raw map[string]json.RawMessage
	// The raw pass detects forbidden authority fields which the typed profile deliberately does not expose.
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// Publication authority belongs to authenticated write paths, so fields
	// ignored by ordinary JSON decoding are rejected rather than accepted.
	for _, key := range []string{"visibility", "provenance", "scope", "public", "owner", "owner_account_id"} {
		// Rejecting any caller-controlled publication field keeps ownership at the authenticated write boundary.
		if _, ok := raw[key]; ok {
			return errors.New("connection profile config cannot set publication controls")
		}
	}
	var decoded profileAlias
	// The strict typed pass closes every nested contract after publication controls have been screened.
	if err := decodeStrictProfileJSON(data, &decoded); err != nil {
		return err
	}
	*p = Profile(decoded)
	return nil
}

// decodeStrictProfileJSON rejects unknown nested fields and trailing values so every Engine ingress observes one closed profile contract.
func decodeStrictProfileJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// Decode exactly one profile document so concatenated JSON cannot hide unaudited configuration.
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return requireProfileJSONEOF(decoder)
}

// requireProfileJSONEOF distinguishes harmless trailing whitespace from a second unaudited JSON value.
func requireProfileJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	// EOF is the only successful completion because the profile contract contains exactly one value.
	if errors.Is(err, io.EOF) {
		return nil
	}
	// A decoder failure still identifies malformed trailing input without exposing profile values.
	if err != nil {
		return fmt.Errorf("invalid trailing connection profile data: %w", err)
	}
	return errors.New("invalid trailing connection profile data")
}

type ResourceDiscoveryConfig struct {
	Version         int      `json:"version" yaml:"version"`
	Stage           string   `json:"stage" yaml:"stage"`
	OperationID     string   `json:"operation_id" yaml:"operation_id"`
	Server          string   `json:"server,omitempty" yaml:"server,omitempty"`
	IDPath          string   `json:"id_path" yaml:"id_path"`
	NamePath        string   `json:"name_path,omitempty" yaml:"name_path,omitempty"`
	BaseURLPath     string   `json:"base_url_path,omitempty" yaml:"base_url_path,omitempty"`
	BaseURLTemplate string   `json:"base_url_template,omitempty" yaml:"base_url_template,omitempty"`
	ScopesPath      string   `json:"scopes_path,omitempty" yaml:"scopes_path,omitempty"`
	ResourceType    string   `json:"resource_type" yaml:"resource_type"`
	AutoRun         string   `json:"auto_run,omitempty" yaml:"auto_run,omitempty"`
	Lifecycle       string   `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"`
	AllowedHosts    []string `json:"allowed_hosts,omitempty" yaml:"allowed_hosts,omitempty"`
}

type ResourceInputConfig struct {
	Fields          []ResourceInputField         `json:"fields" yaml:"fields"`
	BaseURLTemplate string                       `json:"base_url_template" yaml:"base_url_template"`
	ResourceType    string                       `json:"resource_type" yaml:"resource_type"`
	AllowedHosts    []string                     `json:"allowed_hosts,omitempty" yaml:"allowed_hosts,omitempty"`
	DiscoveryMatch  *ResourceInputDiscoveryMatch `json:"discovery_match,omitempty" yaml:"discovery_match,omitempty"`
}

// ResourceInputDiscoveryMatch makes customer input a constraint on provider
// discovery instead of a second competing source of routing records.
type ResourceInputDiscoveryMatch struct {
	MetadataKey string `json:"metadata_key" yaml:"metadata_key"`
}

// ResourceInputFieldType identifies the bounded presentation and validation
// behavior for one customer-supplied routing value.
type ResourceInputFieldType string

const (
	// ResourceInputFieldTypeText preserves the existing free-form string input.
	ResourceInputFieldTypeText ResourceInputFieldType = "text"
	// ResourceInputFieldTypeSelect restricts input to a reviewed option value.
	ResourceInputFieldTypeSelect ResourceInputFieldType = "select"
)

// ResourceInputOption declares one provider-neutral choice without changing
// the string-only transport used by SDK, CLI, and Engine APIs.
type ResourceInputOption struct {
	Value string `json:"value" yaml:"value"`
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
}

// ResourceInputField declares one non-secret customer routing input and its
// bounded hosted-form presentation metadata.
type ResourceInputField struct {
	Name        string                 `json:"name" yaml:"name"`
	Type        ResourceInputFieldType `json:"type,omitempty" yaml:"type,omitempty"`
	Label       string                 `json:"label,omitempty" yaml:"label,omitempty"`
	Placeholder string                 `json:"placeholder,omitempty" yaml:"placeholder,omitempty"`
	Description string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool                   `json:"required,omitempty" yaml:"required,omitempty"`
	Pattern     string                 `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Options     []ResourceInputOption  `json:"options,omitempty" yaml:"options,omitempty"`
}

type Binding struct {
	Value             string   `json:"value" yaml:"value"`
	Location          string   `json:"location" yaml:"location"`
	Name              string   `json:"name,omitempty" yaml:"name,omitempty"`
	Mode              string   `json:"mode" yaml:"mode"`
	Operations        []string `json:"operations,omitempty" yaml:"operations,omitempty"`
	ProviderExtension bool     `json:"provider_extension,omitempty" yaml:"provider_extension,omitempty"`
}

type SourceKind string

const (
	SourceLiteral            SourceKind = "literal"
	SourceEnvironment        SourceKind = "environment"
	SourceConnectionResource SourceKind = "connection_resource"
)

// Expression is the compiled representation persisted by plan/apply. Raw is
// retained for structural plan output; runtime dispatch uses SourcePath.
type Expression struct {
	Kind       SourceKind
	Raw        string
	EnvName    string
	SourcePath string
}

type Operation struct {
	ID         string      `json:"id"`
	Method     string      `json:"method"`
	Parameters []Parameter `json:"parameters,omitempty"`
}

type Parameter struct {
	Name     string `json:"name"`
	Location string `json:"location"`
}

type AuthConfig struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	OAuth2Flows []string `json:"oauth2_flows,omitempty"`
}

// Contract supplies the pinned service-version facts needed for authoritative
// validation without coupling this package to Registry database models.
type Contract struct {
	AuthConfigs []AuthConfig
	Servers     []string
	Operations  []Operation
	Complete    bool
}

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type Issue struct {
	Code     string   `json:"code"`
	Field    string   `json:"field"`
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
}

type Result struct {
	Issues []Issue `json:"issues,omitempty"`
}

// HasErrors lets callers accept warnings without duplicating severity policy.
func (r Result) HasErrors() bool {
	// One blocking issue is sufficient; callers do not need to rescan warnings.
	for _, issue := range r.Issues {
		if issue.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Warnings returns only advisory issues for plan and UI presentation while the
// canonical Result remains unchanged for hashing or logging.
func (r Result) Warnings() []Issue {
	warnings := make([]Issue, 0)
	// Preserve validator order so CLI and UI present deterministic guidance.
	for _, issue := range r.Issues {
		if issue.Severity == SeverityWarning {
			warnings = append(warnings, issue)
		}
	}
	return warnings
}
