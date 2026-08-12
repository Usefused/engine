package ratelimitpolicy

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/shared/strictjson"
)

type Unit string
type Mode string
type IdentityKind string
type Algorithm string
type ResponseSignalSource string
type ResetFormat string

const (
	Version = 3

	ModeEnforce Mode = "enforce"
	ModeObserve Mode = "observe"

	UnitRequests   Unit = "requests"
	UnitPoints     Unit = "points"
	UnitComplexity Unit = "complexity"
	UnitQuotaUnits Unit = "quota_units"

	IdentityAccount                     IdentityKind = "account"
	IdentityServiceVersion              IdentityKind = "service_version"
	IdentityConnection                  IdentityKind = "connection"
	IdentityProject                     IdentityKind = "project"
	IdentityTenant                      IdentityKind = "tenant"
	IdentityResource                    IdentityKind = "resource"
	IdentityIPClass                     IdentityKind = "ip_class"
	IdentityNamedSharedCredentialFamily IdentityKind = "named_shared_credential_family"

	AlgorithmFixedWindow   Algorithm = "fixed_window"
	AlgorithmRollingWindow Algorithm = "rolling_window"
	AlgorithmTokenBucket   Algorithm = "token_bucket"
	AlgorithmConcurrency   Algorithm = "concurrency"

	ResponseSignalHeader ResponseSignalSource = "header"
	ResponseSignalBody   ResponseSignalSource = "body"

	ResetDeltaSeconds      ResetFormat = "delta_seconds"
	ResetDeltaMilliseconds ResetFormat = "delta_milliseconds"
	ResetUnixSeconds       ResetFormat = "unix_seconds"
	ResetUnixMilliseconds  ResetFormat = "unix_milliseconds"
	ResetRFC3339           ResetFormat = "rfc3339"
	ResetHTTPDate          ResetFormat = "http_date"
)

const MaxRuntimePolicyValue = maxPolicyValue

const (
	MaxPolicies    = 16
	maxPolicyValue = int64(1_000_000_000_000)
	maxIntervalMS  = int64(2_678_400_000)
)

var (
	policyNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

type Config struct {
	Version  int       `json:"version" yaml:"version"`
	Policies []Policy  `json:"policies" yaml:"policies"`
	Cooldown *Cooldown `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`
}

type Policy struct {
	Name      string         `json:"name" yaml:"name"`
	Mode      Mode           `json:"mode" yaml:"mode"`
	Unit      Unit           `json:"unit" yaml:"unit"`
	Identity  BucketIdentity `json:"identity" yaml:"identity"`
	Cost      CostPlan       `json:"cost" yaml:"cost"`
	Algorithm Algorithm      `json:"algorithm" yaml:"algorithm"`

	FixedWindow     *FixedWindow     `json:"fixed_window,omitempty" yaml:"fixed_window,omitempty"`
	RollingWindow   *RollingWindow   `json:"rolling_window,omitempty" yaml:"rolling_window,omitempty"`
	TokenBucket     *TokenBucket     `json:"token_bucket,omitempty" yaml:"token_bucket,omitempty"`
	Concurrency     *Concurrency     `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	ResponseSignals *ResponseSignals `json:"response_signals,omitempty" yaml:"response_signals,omitempty"`
}

type BucketIdentity struct {
	Inputs []IdentityInput `json:"inputs" yaml:"inputs"`
}

type IdentityInput struct {
	Kind    IdentityKind `json:"kind" yaml:"kind"`
	Binding string       `json:"binding,omitempty" yaml:"binding,omitempty"`
	Name    string       `json:"name,omitempty" yaml:"name,omitempty"`
}

type CostPlan struct {
	Default int64      `json:"default" yaml:"default"`
	Rules   []CostRule `json:"rules" yaml:"rules"`
}

type CostRule struct {
	Operation string `json:"operation" yaml:"operation"`
	Cost      int64  `json:"cost" yaml:"cost"`
}

type FixedWindow struct {
	Limit      int64 `json:"limit" yaml:"limit"`
	DurationMs int64 `json:"duration_ms" yaml:"duration_ms"`
}

type RollingWindow struct {
	Limit      int64 `json:"limit" yaml:"limit"`
	DurationMs int64 `json:"duration_ms" yaml:"duration_ms"`
}

type TokenBucket struct {
	Capacity         int64 `json:"capacity" yaml:"capacity"`
	RefillUnits      int64 `json:"refill_units" yaml:"refill_units"`
	RefillIntervalMs int64 `json:"refill_interval_ms" yaml:"refill_interval_ms"`
}

type Concurrency struct {
	Limit int64 `json:"limit" yaml:"limit"`
}

type ResponseSignals struct {
	Limit     *ResponseSignal `json:"limit,omitempty" yaml:"limit,omitempty"`
	Remaining *ResponseSignal `json:"remaining,omitempty" yaml:"remaining,omitempty"`
	Reset     *ResetSignal    `json:"reset,omitempty" yaml:"reset,omitempty"`
	Cost      *ResponseSignal `json:"cost,omitempty" yaml:"cost,omitempty"`
}

type ResponseSignal struct {
	Source ResponseSignalSource `json:"source" yaml:"source"`
	Name   string               `json:"name,omitempty" yaml:"name,omitempty"`
	Path   string               `json:"path,omitempty" yaml:"path,omitempty"`
}

type ResetSignal struct {
	Signal ResponseSignal `json:"signal" yaml:"signal"`
	Format ResetFormat    `json:"format" yaml:"format"`
}

type Cooldown struct {
	Statuses []StatusRange    `json:"statuses" yaml:"statuses"`
	Headers  []CooldownHeader `json:"headers" yaml:"headers"`
}

type StatusRange struct {
	Min int `json:"min" yaml:"min"`
	Max int `json:"max" yaml:"max"`
}

type CooldownHeader struct {
	Name       string        `json:"name" yaml:"name"`
	Formats    []ResetFormat `json:"formats" yaml:"formats"`
	MaxDelayMs int64         `json:"max_delay_ms" yaml:"max_delay_ms"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var decoded plain
	if err := strictjson.Decode(data, &decoded, "rate limit config"); err != nil {
		return err
	}
	*c = Config(decoded)
	return c.Validate()
}

// Value validates before persistence so invalid or legacy shapes cannot be
// introduced through a locally constructed Config that bypassed JSON decode.
func (c Config) Value() (driver.Value, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// Scan keeps database reads on the same strict contract boundary as Registry
// and workspace JSON; accepting an old shape here would silently disable limits.
func (c *Config) Scan(value interface{}) error {
	if value == nil {
		*c = Config{}
		return nil
	}
	data, err := jsonBytes(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, c)
}

func jsonBytes(value interface{}) ([]byte, error) {
	switch typed := value.(type) {
	case []byte:
		return typed, nil
	case string:
		return []byte(typed), nil
	default:
		return nil, errors.New("rate limit config requires JSON bytes or string")
	}
}

func (c Config) Validate() error {
	return validateV3Config(c)
}

func validOperationKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len(key) <= 512 &&
		!strings.ContainsAny(key, "\x00\r\n")
}

func validHeaderName(value string) bool {
	return value != "" && len(value) <= 256 && headerNamePattern.MatchString(value)
}

func validPolicyValue(value int64) bool {
	return value > 0 && value <= maxPolicyValue
}

func validInterval(value int64) bool {
	return value >= 1 && value <= maxIntervalMS
}

func (p Policy) ResolvedCost(stableOperationKey string) int64 {
	for _, rule := range p.Cost.Rules {
		if rule.Operation == stableOperationKey {
			return rule.Cost
		}
	}
	return p.Cost.Default
}
