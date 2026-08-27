// Package paginationpolicy owns the provider-neutral pagination contract used
// at the Engine's Registry, workspace, persistence, and execution boundaries.
package paginationpolicy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/shared/strictjson"
)

type RequestLocation string
type SourceLocation string
type ValueType string
type IncrementMode string
type PageApplication string
type ConditionOperator string
type ItemPosition string
type ContinuationKind string
type OriginMode string
type RepeatedValueBehavior string

const (
	Version = 3

	RequestQuery           RequestLocation = "query"
	RequestHeader          RequestLocation = "header"
	RequestBody            RequestLocation = "body"
	RequestGraphQLVariable RequestLocation = "graphql_variable"

	SourceBody    SourceLocation = "body"
	SourceHeader  SourceLocation = "header"
	SourceLink    SourceLocation = "link"
	SourceItems   SourceLocation = "items"
	SourceGraphQL SourceLocation = "graphql"

	ValueString  ValueType = "string"
	ValueInteger ValueType = "integer"
	ValueBoolean ValueType = "boolean"
	ValueURL     ValueType = "url"

	IncrementFixed         IncrementMode = "fixed"
	IncrementItemsReturned IncrementMode = "items_returned"

	ApplyAll        PageApplication = "all"
	ApplyFirst      PageApplication = "first"
	ApplySubsequent PageApplication = "subsequent"

	ConditionEquals    ConditionOperator = "equals"
	ConditionNotEquals ConditionOperator = "not_equals"
	ConditionPresent   ConditionOperator = "present"
	ConditionAbsent    ConditionOperator = "absent"
	ConditionStateGTE  ConditionOperator = "state_gte"

	ItemLast ItemPosition = "last"

	ContinuationToken   ContinuationKind = "token"
	ContinuationOffset  ContinuationKind = "offset"
	ContinuationPage    ContinuationKind = "page"
	ContinuationRFCLink ContinuationKind = "rfc_link"
	ContinuationNextURL ContinuationKind = "next_url"

	OriginSame OriginMode = "same_origin"
	OriginList OriginMode = "allowlist"

	RepeatedStop  RepeatedValueBehavior = "stop"
	RepeatedError RepeatedValueBehavior = "error"

	DefaultMaxPages      = 100
	DefaultMaxItems      = int64(10_000)
	DefaultMaxBytes      = int64(16_777_216)
	DefaultMaxDurationMs = int64(120_000)

	CeilingMaxPages      = 1_000
	CeilingMaxItems      = int64(100_000)
	CeilingMaxBytes      = int64(67_108_864)
	CeilingMaxDurationMs = int64(300_000)

	maxPathLength   = 512
	maxPathSegments = 32
	maxNameLength   = 128
)

var (
	ErrInvalid      = errors.New("invalid pagination policy")
	bodyPathPattern = regexp.MustCompile(`^\$(?:\.[A-Za-z_][A-Za-z0-9_-]*)+$`)
	headerPattern   = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
)

type Config struct {
	Version      int                `json:"version"`
	Request      []RequestStep      `json:"request"`
	Response     ResponsePlan       `json:"response"`
	Continuation []ContinuationStep `json:"continuation"`
	Termination  Termination        `json:"termination"`
	GraphQL      *GraphQLPlan       `json:"graphql,omitempty"`
	Limits       Limits             `json:"limits"`
}

type RequestTarget struct {
	Location RequestLocation `json:"location"`
	Name     string          `json:"name"`
}

type ValueSource struct {
	Location  SourceLocation    `json:"location"`
	Path      string            `json:"path,omitempty"`
	Name      string            `json:"name,omitempty"`
	Relation  string            `json:"relation,omitempty"`
	ValueType ValueType         `json:"value_type"`
	Paths     []ConditionalPath `json:"paths,omitempty"`
	Item      *ItemSelector     `json:"item,omitempty"`
}

type Scalar struct {
	Type    ValueType `json:"type"`
	String  *string   `json:"string,omitempty"`
	Integer *int64    `json:"integer,omitempty"`
	Boolean *bool     `json:"boolean,omitempty"`
}

type RequestStep struct {
	State     string          `json:"state,omitempty"`
	Target    RequestTarget   `json:"target"`
	ValueType ValueType       `json:"value_type"`
	Initial   *Scalar         `json:"initial,omitempty"`
	Constant  *Scalar         `json:"constant,omitempty"`
	Apply     PageApplication `json:"apply"`
}

type ResponsePlan struct {
	Items  ItemsSource     `json:"items"`
	Values []ResponseValue `json:"values"`
}

type ItemsSource struct {
	Path           string            `json:"path,omitempty"`
	Paths          []ConditionalPath `json:"paths,omitempty"`
	MissingIsEmpty bool              `json:"missing_is_empty,omitempty"`
}

type ResponseValue struct {
	Name   string      `json:"name"`
	Source ValueSource `json:"source"`
}

type ConditionalPath struct {
	Path string           `json:"path"`
	When RequestCondition `json:"when"`
}

type RequestCondition struct {
	State    string            `json:"state"`
	Operator ConditionOperator `json:"operator"`
	Value    *Scalar           `json:"value,omitempty"`
}

type ItemSelector struct {
	Position ItemPosition `json:"position"`
	Path     string       `json:"path,omitempty"`
}

type ContinuationStep struct {
	Kind          ContinuationKind `json:"kind"`
	State         string           `json:"state"`
	ResponseValue string           `json:"response_value,omitempty"`
	Increment     *Increment       `json:"increment,omitempty"`
	Origin        *OriginPolicy    `json:"origin,omitempty"`
}

type Increment struct {
	Mode  IncrementMode `json:"mode"`
	Value int64         `json:"value,omitempty"`
}

type OriginPolicy struct {
	Mode           OriginMode `json:"mode"`
	AllowedOrigins []string   `json:"allowed_origins,omitempty"`
}

type Termination struct {
	StopOnEmptyItems    bool                  `json:"stop_on_empty_items,omitempty"`
	StopOnShortPage     *ShortPageTermination `json:"stop_on_short_page,omitempty"`
	StopOnMissingValues []string              `json:"stop_on_missing_values,omitempty"`
	Conditions          []ResponseCondition   `json:"conditions,omitempty"`
	RepeatedValue       RepeatedValueBehavior `json:"repeated_value"`
}

type ShortPageTermination struct {
	RequestState string `json:"request_state"`
}

type ResponseCondition struct {
	ResponseValue string            `json:"response_value"`
	State         string            `json:"state,omitempty"`
	Operator      ConditionOperator `json:"operator"`
	Value         *Scalar           `json:"value,omitempty"`
}

type GraphQLPlan struct {
	Variables              []GraphQLVariable    `json:"variables"`
	ResultAliases          []GraphQLResultAlias `json:"result_aliases"`
	FirstPageTemplate      string               `json:"first_page_template"`
	SubsequentPageTemplate string               `json:"subsequent_page_template"`
}

type GraphQLVariable struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	ValueType ValueType `json:"value_type"`
}

type GraphQLResultAlias struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

type Limits struct {
	MaxPages      int   `json:"max_pages"`
	MaxItems      int64 `json:"max_items"`
	MaxBytes      int64 `json:"max_bytes"`
	MaxDurationMs int64 `json:"max_duration_ms"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var decoded plain
	if err := strictjson.Decode(data, &decoded, "pagination config"); err != nil {
		return err
	}
	*c = Config(decoded)
	return Validate(c)
}

func EffectiveLimits(value Limits) Limits {
	return Limits{
		MaxPages:      defaultInt(value.MaxPages, DefaultMaxPages),
		MaxItems:      defaultInt64(value.MaxItems, DefaultMaxItems),
		MaxBytes:      defaultInt64(value.MaxBytes, DefaultMaxBytes),
		MaxDurationMs: defaultInt64(value.MaxDurationMs, DefaultMaxDurationMs),
	}
}

func Validate(config *Config) error {
	if config == nil {
		return invalid("policy is required")
	}
	if config.Version != Version {
		return invalid("pagination version is unsupported")
	}
	return validateV3(config)
}

// ValidateItemsPath accepts the document root because some provider list
// operations return a JSON array without an enclosing response object.
func ValidateItemsPath(path string) error {
	if path == "$" {
		return nil
	}
	return ValidateBodyPath(path)
}

func ValidateBodyPath(path string) error {
	if path == "" || len(path) > maxPathLength || !bodyPathPattern.MatchString(path) {
		return errors.New("must be a bounded body JSON path")
	}
	if strings.Count(path, ".") > maxPathSegments {
		return fmt.Errorf("must contain at most %d segments", maxPathSegments)
	}
	return nil
}

func validateLimits(value Limits) error {
	if value.MaxPages < 1 || value.MaxPages > CeilingMaxPages {
		return invalid("max_pages is outside the safe range")
	}
	if value.MaxItems < 1 || value.MaxItems > CeilingMaxItems {
		return invalid("max_items is outside the safe range")
	}
	if value.MaxBytes < 1 || value.MaxBytes > CeilingMaxBytes {
		return invalid("max_bytes is outside the safe range")
	}
	if value.MaxDurationMs < 1 || value.MaxDurationMs > CeilingMaxDurationMs {
		return invalid("max_duration_ms is outside the safe range")
	}
	return nil
}

func validateRequestTarget(value RequestTarget) error {
	if value.Name == "" || len(value.Name) > maxNameLength || strings.ContainsAny(value.Name, "\r\n") {
		return invalid("request target name is invalid")
	}
	if value.Location != RequestQuery && value.Location != RequestHeader && value.Location != RequestBody {
		return invalid("unsupported request target location")
	}
	if value.Location == RequestHeader && !headerPattern.MatchString(value.Name) {
		return invalid("request header name is invalid")
	}
	return nil
}

func validateScalar(value *Scalar) error {
	if value == nil {
		return nil
	}
	if paginationScalarValueCount(value) == 1 && paginationScalarMatchesType(value) {
		return nil
	}
	return invalid("initial cursor must contain exactly one typed scalar")
}

func paginationScalarValueCount(value *Scalar) int {
	count := 0
	for _, present := range []bool{value.String != nil, value.Integer != nil, value.Boolean != nil} {
		if present {
			count++
		}
	}
	return count
}

func paginationScalarMatchesType(value *Scalar) bool {
	switch value.Type {
	case ValueString, ValueURL:
		return value.String != nil
	case ValueInteger:
		return value.Integer != nil
	case ValueBoolean:
		return value.Boolean != nil
	default:
		return false
	}
}

func validateOptionalSource(value *ValueSource, allowed ...ValueType) error {
	if value == nil {
		return nil
	}
	return validateSource(*value, allowed...)
}

func validateSource(value ValueSource, allowed ...ValueType) error {
	if !containsValueType(allowed, value.ValueType) {
		return invalid("source value_type is invalid")
	}
	switch value.Location {
	case SourceBody:
		return validateBodySource(value)
	case SourceHeader:
		return validateHeaderSource(value)
	case SourceLink:
		return validateLinkSource(value)
	default:
		return invalid("unsupported source location")
	}
}

func validateBodySource(value ValueSource) error {
	if value.Name != "" || value.Relation != "" {
		return invalid("body source only accepts path")
	}
	if err := ValidateBodyPath(value.Path); err != nil {
		return invalid(err.Error())
	}
	return nil
}

func validateHeaderSource(value ValueSource) error {
	if value.Path != "" || value.Relation != "" || !validHeaderName(value.Name) {
		return invalid("header source only accepts a valid name")
	}
	return nil
}

func validateLinkSource(value ValueSource) error {
	valid := value.Path == "" && len(value.Paths) == 0 && value.Item == nil && validHeaderName(value.Name) && value.Relation == "next" && value.ValueType == ValueURL
	if !valid {
		return invalid("link source requires a header name, relation next, and URL value")
	}
	return nil
}

func anyTrue(values []bool) bool {
	for _, value := range values {
		if value {
			return true
		}
	}
	return false
}

func validHeaderName(name string) bool {
	return name != "" && len(name) <= maxNameLength && headerPattern.MatchString(name)
}

func containsValueType(values []ValueType, wanted ValueType) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func defaultInt64(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }
