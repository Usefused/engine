// Package connectionprofile owns the provider-neutral connection profile
// contract shared by every Registry and Engine ingress.
package connectionprofile

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Profile is the canonical, versioned provider behavior attached to a bucket.
// Publication provenance and visibility intentionally live outside this
// caller-controlled configuration object.
type Profile struct {
	AuthType          string                   `json:"auth_type,omitempty" yaml:"auth_type,omitempty"`
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
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// Publication authority belongs to authenticated write paths, so fields
	// ignored by ordinary JSON decoding are rejected rather than accepted.
	for _, key := range []string{"visibility", "provenance", "scope", "public", "owner", "owner_account_id"} {
		if _, ok := raw[key]; ok {
			return errors.New("connection profile config cannot set publication controls")
		}
	}
	var decoded profileAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Profile(decoded)
	return nil
}

type ResourceDiscoveryConfig struct {
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
	Fields          []ResourceInputField `json:"fields" yaml:"fields"`
	BaseURLTemplate string               `json:"base_url_template" yaml:"base_url_template"`
	ResourceType    string               `json:"resource_type" yaml:"resource_type"`
	AllowedHosts    []string             `json:"allowed_hosts,omitempty" yaml:"allowed_hosts,omitempty"`
}

type ResourceInputField struct {
	Name     string `json:"name" yaml:"name"`
	Label    string `json:"label,omitempty" yaml:"label,omitempty"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Pattern  string `json:"pattern,omitempty" yaml:"pattern,omitempty"`
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

// Contract supplies the pinned service-version facts needed for authoritative
// validation without coupling this package to Registry database models.
type Contract struct {
	AuthTypes  []string
	Servers    []string
	Operations []Operation
	Complete   bool
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

type Revision struct {
	ID               uuid.UUID `json:"id"`
	ProfileID        uuid.UUID `json:"profile_id"`
	ServiceID        uuid.UUID `json:"service_id"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	OwnerAccountID   uuid.UUID `json:"owner_account_id"`
	Name             string    `json:"name"`
	AuthType         string    `json:"auth_type"`
	Revision         int       `json:"revision"`
	ProfileHash      string    `json:"profile_hash"`
	Config           Profile   `json:"config"`
	Provenance       string    `json:"provenance"`
	Visibility       string    `json:"visibility"`
	CreatedBy        uuid.UUID `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	PublishedAt      time.Time `json:"published_at"`
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
