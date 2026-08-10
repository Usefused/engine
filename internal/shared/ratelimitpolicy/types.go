package ratelimitpolicy

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const Version = 2

const (
	MaxPolicies       = 16
	maxPolicyValue    = int64(1_000_000_000_000)
	maxIntervalMS     = int64(2_678_400_000)
	maxOperationCosts = 10_000
)

var (
	policyNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

type Config struct {
	Version    int         `json:"version"`
	Policies   []Policy    `json:"policies"`
	RetryAfter *RetryAfter `json:"retry_after,omitempty"`
}

type Policy struct {
	Name            string           `json:"name"`
	Unit            string           `json:"unit"`
	Scope           string           `json:"scope"`
	DefaultCost     int64            `json:"default_cost"`
	OperationCosts  map[string]int64 `json:"operation_costs"`
	Algorithm       string           `json:"algorithm"`
	FixedWindow     *FixedWindow     `json:"fixed_window,omitempty"`
	TokenBucket     *TokenBucket     `json:"token_bucket,omitempty"`
	ResponseHeaders *ResponseHeaders `json:"response_headers,omitempty"`
}

type FixedWindow struct {
	Limit      int64 `json:"limit"`
	DurationMS int64 `json:"duration_ms"`
}

type TokenBucket struct {
	Capacity         int64 `json:"capacity"`
	RefillUnits      int64 `json:"refill_units"`
	RefillIntervalMS int64 `json:"refill_interval_ms"`
}

type ResponseHeaders struct {
	Limit     string       `json:"limit,omitempty"`
	Remaining string       `json:"remaining,omitempty"`
	Reset     *ResetHeader `json:"reset,omitempty"`
}

type ResetHeader struct {
	Name   string `json:"name"`
	Format string `json:"format"`
}

type RetryAfter struct {
	Enabled    bool  `json:"enabled"`
	MaxDelayMS int64 `json:"max_delay_ms"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded plain
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("rate limit config contains trailing data")
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
	if c.Version != Version || len(c.Policies) == 0 || len(c.Policies) > MaxPolicies {
		return errors.New("rate limit v2 config is invalid")
	}
	seen := make(map[string]struct{}, len(c.Policies))
	for _, policy := range c.Policies {
		if err := validatePolicy(policy, seen); err != nil {
			return err
		}
	}
	if c.RetryAfter != nil && (!c.RetryAfter.Enabled || c.RetryAfter.MaxDelayMS < 1 || c.RetryAfter.MaxDelayMS > 86_400_000) {
		return errors.New("rate limit retry_after is invalid")
	}
	return nil
}

func validatePolicy(policy Policy, seen map[string]struct{}) error {
	if err := validatePolicyIdentity(policy, seen); err != nil {
		return err
	}
	if err := validatePolicyCosts(policy); err != nil {
		return err
	}
	if err := validateAlgorithm(policy); err != nil {
		return fmt.Errorf("rate limit policy %q: %w", policy.Name, err)
	}
	return validateResponseHeaders(policy.ResponseHeaders)
}

func validatePolicyIdentity(policy Policy, seen map[string]struct{}) error {
	if policy.Name == "" || len(policy.Name) > 64 || !policyNamePattern.MatchString(policy.Name) {
		return errors.New("rate limit policy name is invalid")
	}
	if _, exists := seen[policy.Name]; exists {
		return errors.New("rate limit policy name is duplicated")
	}
	seen[policy.Name] = struct{}{}
	if !validUnit(policy.Unit) || !validScope(policy.Scope) {
		return errors.New("rate limit policy unit or scope is invalid")
	}
	return nil
}

func validatePolicyCosts(policy Policy) error {
	if policy.DefaultCost < 0 || policy.DefaultCost > maxPolicyValue || len(policy.OperationCosts) > maxOperationCosts {
		return errors.New("rate limit policy cost is invalid")
	}
	positiveCost := policy.DefaultCost > 0
	for key, cost := range policy.OperationCosts {
		if !validOperationKey(key) || cost < 0 || cost > maxPolicyValue {
			return errors.New("rate limit operation cost is invalid")
		}
		positiveCost = positiveCost || cost > 0
	}
	if !positiveCost {
		return errors.New("rate limit policy requires a positive cost")
	}
	return nil
}

func validUnit(unit string) bool {
	switch unit {
	case "requests", "points", "quota_units":
		return true
	default:
		return false
	}
}

func validScope(scope string) bool {
	return scope == "service_version" || scope == "connection"
}

func validOperationKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len(key) <= 512 &&
		!strings.ContainsAny(key, "\x00\r\n")
}

func validateAlgorithm(policy Policy) error {
	switch policy.Algorithm {
	case "fixed_window":
		return validateFixedWindow(policy)
	case "token_bucket":
		return validateTokenBucket(policy)
	default:
		return errors.New("algorithm is invalid")
	}
}

func validateFixedWindow(policy Policy) error {
	if policy.FixedWindow == nil || policy.TokenBucket != nil {
		return errors.New("fixed_window discriminator is invalid")
	}
	if !validPolicyValue(policy.FixedWindow.Limit) || !validInterval(policy.FixedWindow.DurationMS) {
		return errors.New("fixed_window discriminator is invalid")
	}
	return nil
}

func validateTokenBucket(policy Policy) error {
	if policy.TokenBucket == nil || policy.FixedWindow != nil {
		return errors.New("token_bucket discriminator is invalid")
	}
	if !validPolicyValue(policy.TokenBucket.Capacity) || !validPolicyValue(policy.TokenBucket.RefillUnits) || !validInterval(policy.TokenBucket.RefillIntervalMS) {
		return errors.New("token_bucket discriminator is invalid")
	}
	return nil
}

func validateResponseHeaders(headers *ResponseHeaders) error {
	if headers == nil {
		return nil
	}
	if headers.Limit == "" && headers.Remaining == "" && headers.Reset == nil {
		return errors.New("rate limit response headers are empty")
	}
	if !validOptionalHeaderName(headers.Limit) || !validOptionalHeaderName(headers.Remaining) {
		return errors.New("rate limit response header is invalid")
	}
	if headers.Reset == nil {
		return nil
	}
	return validateResetHeader(headers.Reset)
}

func validateResetHeader(reset *ResetHeader) error {
	if !validHeaderName(reset.Name) {
		return errors.New("rate limit reset header is invalid")
	}
	switch reset.Format {
	case "delta_seconds", "unix_seconds", "unix_milliseconds", "rfc3339":
		return nil
	default:
		return errors.New("rate limit reset header format is invalid")
	}
}

func validOptionalHeaderName(value string) bool {
	return value == "" || validHeaderName(value)
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

func (p Policy) Cost(stableOperationKey string) int64 {
	if cost, ok := p.OperationCosts[stableOperationKey]; ok {
		return cost
	}
	return p.DefaultCost
}
