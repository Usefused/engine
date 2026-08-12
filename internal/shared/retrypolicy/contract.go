package retrypolicy

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/shared/strictjson"
)

type OperationKind string
type ErrorKind string
type BodyReplayability string
type IdempotencyKeyRequirement string
type BackoffStrategy string
type RetryAfterFormat string

const (
	Version = 3

	OperationRead     OperationKind = "read"
	OperationWrite    OperationKind = "write"
	OperationDelete   OperationKind = "delete"
	OperationStream   OperationKind = "stream"
	OperationQuery    OperationKind = "query"
	OperationMutation OperationKind = "mutation"

	ErrorConnectTimeout  ErrorKind = "connect_timeout"
	ErrorReadTimeout     ErrorKind = "read_timeout"
	ErrorConnectionReset ErrorKind = "connection_reset"
	ErrorTemporaryDNS    ErrorKind = "temporary_dns"
	ErrorTLSHandshake    ErrorKind = "tls_handshake"
	ErrorTransport       ErrorKind = "transport"

	BodyAny           BodyReplayability = "any"
	BodyReplayable    BodyReplayability = "replayable"
	BodyNotReplayable BodyReplayability = "not_replayable"

	IdempotencyKeyAny      IdempotencyKeyRequirement = "any"
	IdempotencyKeyRequired IdempotencyKeyRequirement = "required"
	IdempotencyKeyAbsent   IdempotencyKeyRequirement = "absent"

	BackoffFixed       BackoffStrategy = "fixed"
	BackoffExponential BackoffStrategy = "exponential"

	RetryAfterDeltaSeconds     RetryAfterFormat = "delta_seconds"
	RetryAfterUnixSeconds      RetryAfterFormat = "unix_seconds"
	RetryAfterUnixMilliseconds RetryAfterFormat = "unix_milliseconds"
	RetryAfterRFC3339          RetryAfterFormat = "rfc3339"
	RetryAfterHTTPDate         RetryAfterFormat = "http_date"
)

type Config struct {
	Version int    `json:"version" yaml:"version"`
	Rules   []Rule `json:"rules" yaml:"rules"`
}

type Rule struct {
	Predicates Predicates `json:"predicates" yaml:"predicates"`
	Action     Action     `json:"action" yaml:"action"`
}
type Predicates struct {
	Methods                 []string                `json:"methods" yaml:"methods"`
	OperationKinds          []OperationKind         `json:"operation_kinds" yaml:"operation_kinds"`
	Statuses                []StatusRange           `json:"statuses" yaml:"statuses"`
	Errors                  []ErrorKind             `json:"errors" yaml:"errors"`
	BodyReplayability       BodyReplayability       `json:"body_replayability" yaml:"body_replayability"`
	IdempotencyKey          IdempotencyKeyPredicate `json:"idempotency_key" yaml:"idempotency_key"`
	RequiredProviderHeaders []string                `json:"required_provider_headers" yaml:"required_provider_headers"`
}
type StatusRange struct {
	Min int `json:"min" yaml:"min"`
	Max int `json:"max" yaml:"max"`
}
type IdempotencyKeyPredicate struct {
	Requirement IdempotencyKeyRequirement `json:"requirement" yaml:"requirement"`
	Header      string                    `json:"header,omitempty" yaml:"header,omitempty"`
}
type Action struct {
	MaxAttempts       int                `json:"max_attempts" yaml:"max_attempts"`
	MaxElapsedMs      int64              `json:"max_elapsed_ms" yaml:"max_elapsed_ms"`
	Backoff           Backoff            `json:"backoff" yaml:"backoff"`
	RetryAfterHeaders []RetryAfterHeader `json:"retry_after_headers" yaml:"retry_after_headers"`
}
type Backoff struct {
	Strategy    BackoffStrategy `json:"strategy" yaml:"strategy"`
	BaseDelayMs int64           `json:"base_delay_ms" yaml:"base_delay_ms"`
	MaxDelayMs  int64           `json:"max_delay_ms" yaml:"max_delay_ms"`
	JitterMs    int64           `json:"jitter_ms" yaml:"jitter_ms"`
}
type RetryAfterHeader struct {
	Name       string             `json:"name" yaml:"name"`
	Formats    []RetryAfterFormat `json:"formats" yaml:"formats"`
	MaxDelayMs int64              `json:"max_delay_ms" yaml:"max_delay_ms"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var decoded plain
	if err := strictjson.Decode(data, &decoded, "retry config"); err != nil {
		return err
	}
	*c = Config(decoded)
	return Validate(c)
}

func (c Config) Value() (driver.Value, error) {
	if err := Validate(&c); err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

func (c *Config) Scan(value interface{}) error {
	if value == nil {
		*c = Config{}
		return nil
	}
	var payload []byte
	switch typed := value.(type) {
	case []byte:
		payload = typed
	case string:
		payload = []byte(typed)
	default:
		return errors.New("retry config requires JSON bytes or string")
	}
	if err := json.Unmarshal(payload, c); err != nil {
		return err
	}
	return Validate(c)
}

const (
	maxRules           = 32
	maxPredicateValues = 32
	maxAttempts        = 16
	maxElapsedMs       = int64(300_000)
	maxDelayMs         = int64(86_400_000)
)

// Validate accepts only exact v3 at the Engine boundary because Registry owns
// all legacy retry normalization before snapshot publication.
func Validate(config *Config) error {
	if config == nil {
		return nil
	}
	return validateV3Exact(config)
}

func validBodyReplayability(v BodyReplayability) bool {
	return v == BodyAny || v == BodyReplayable || v == BodyNotReplayable
}
func validRetryHeader(v string) bool {
	return v != "" && len(v) <= 256 && !strings.ContainsAny(v, "\r\n")
}

func validRetryAfterHeader(header RetryAfterHeader) bool {
	if !validRetryAfterHeaderBounds(header) {
		return false
	}
	for _, format := range header.Formats {
		if !validRetryAfterFormat(format) {
			return false
		}
	}
	return true
}

func validRetryAfterHeaderBounds(header RetryAfterHeader) bool {
	return validRetryHeader(header.Name) && len(header.Formats) > 0 && len(header.Formats) <= 8 && header.MaxDelayMs >= 1 && header.MaxDelayMs <= maxDelayMs
}

func validRetryAfterFormat(format RetryAfterFormat) bool {
	switch format {
	case RetryAfterDeltaSeconds, RetryAfterUnixSeconds, RetryAfterUnixMilliseconds, RetryAfterRFC3339, RetryAfterHTTPDate:
		return true
	default:
		return false
	}
}
