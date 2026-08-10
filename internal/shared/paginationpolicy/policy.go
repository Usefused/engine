// Package paginationpolicy owns the provider-neutral pagination contract used
// at the Engine's Registry, workspace, persistence, and execution boundaries.
package paginationpolicy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type Type string
type RequestLocation string
type SourceLocation string
type ValueType string
type IncrementMode string

const (
	Version  = 2
	Version2 = Version

	TypeCursor     Type = "cursor"
	TypeOffset     Type = "offset"
	TypePageNumber Type = "page_number"
	TypeNextURL    Type = "next_url"

	RequestQuery  RequestLocation = "query"
	RequestHeader RequestLocation = "header"
	RequestBody   RequestLocation = "body"

	SourceBody   SourceLocation = "body"
	SourceHeader SourceLocation = "header"
	SourceLink   SourceLocation = "link"

	ValueString  ValueType = "string"
	ValueInteger ValueType = "integer"
	ValueBoolean ValueType = "boolean"
	ValueURL     ValueType = "url"

	IncrementFixed         IncrementMode = "fixed"
	IncrementItemsReturned IncrementMode = "items_returned"

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
	Version    int               `json:"version"`
	Type       Type              `json:"type"`
	Cursor     *CursorConfig     `json:"cursor,omitempty"`
	Offset     *OffsetConfig     `json:"offset,omitempty"`
	PageNumber *PageNumberConfig `json:"page_number,omitempty"`
	NextURL    *NextURLConfig    `json:"next_url,omitempty"`
	ItemsPath  string            `json:"items_path"`
	Limits     Limits            `json:"limits"`
}

type RequestTarget struct {
	Location RequestLocation `json:"location"`
	Name     string          `json:"name"`
}

type ValueSource struct {
	Location  SourceLocation `json:"location"`
	Path      string         `json:"path,omitempty"`
	Name      string         `json:"name,omitempty"`
	Relation  string         `json:"relation,omitempty"`
	ValueType ValueType      `json:"value_type"`
}

type Scalar struct {
	Type    ValueType `json:"type"`
	String  *string   `json:"string,omitempty"`
	Integer *int64    `json:"integer,omitempty"`
}

type CursorConfig struct {
	Request RequestTarget `json:"request"`
	Initial *Scalar       `json:"initial,omitempty"`
	Next    ValueSource   `json:"next"`
	HasMore *ValueSource  `json:"has_more,omitempty"`
}

type OffsetIncrement struct {
	Mode  IncrementMode `json:"mode"`
	Value int64         `json:"value,omitempty"`
}

type PageSize struct {
	Target RequestTarget `json:"target"`
	Value  int64         `json:"value"`
}

type OffsetConfig struct {
	Request         RequestTarget   `json:"request"`
	Start           int64           `json:"start"`
	Increment       OffsetIncrement `json:"increment"`
	PageSize        *PageSize       `json:"page_size,omitempty"`
	NextOffset      *ValueSource    `json:"next_offset,omitempty"`
	TotalItems      *ValueSource    `json:"total_items,omitempty"`
	HasMore         *ValueSource    `json:"has_more,omitempty"`
	StopOnShortPage bool            `json:"stop_on_short_page,omitempty"`
}

type PageNumberConfig struct {
	Request         RequestTarget `json:"request"`
	Start           int64         `json:"start"`
	Increment       int64         `json:"increment"`
	PageSize        *PageSize     `json:"page_size,omitempty"`
	TotalPages      *ValueSource  `json:"total_pages,omitempty"`
	HasMore         *ValueSource  `json:"has_more,omitempty"`
	StopOnShortPage bool          `json:"stop_on_short_page,omitempty"`
}

type NextURLConfig struct {
	Next ValueSource `json:"next"`
}

type Limits struct {
	MaxPages      int   `json:"max_pages"`
	MaxItems      int64 `json:"max_items"`
	MaxBytes      int64 `json:"max_bytes"`
	MaxDurationMs int64 `json:"max_duration_ms"`
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
		return invalid("pagination version must be 2")
	}
	if err := ValidateBodyPath(config.ItemsPath); err != nil {
		return invalid("items_path: " + err.Error())
	}
	if err := validateLimits(EffectiveLimits(config.Limits)); err != nil {
		return err
	}
	return validateStrategy(config)
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

func validateStrategy(config *Config) error {
	if branchCount(config) != 1 {
		return invalid("exactly one strategy branch is required")
	}
	switch config.Type {
	case TypeCursor:
		return validateCursor(config.Cursor)
	case TypeOffset:
		return validateOffset(config.Offset, config.ItemsPath)
	case TypePageNumber:
		return validatePageNumber(config.PageNumber, config.ItemsPath)
	case TypeNextURL:
		return validateNextURL(config.NextURL)
	default:
		return invalid("unsupported type")
	}
}

func branchCount(config *Config) int {
	count := 0
	for _, present := range []bool{config.Cursor != nil, config.Offset != nil, config.PageNumber != nil, config.NextURL != nil} {
		if present {
			count++
		}
	}
	return count
}

func validateCursor(config *CursorConfig) error {
	if config == nil {
		return invalid("cursor strategy is required")
	}
	if err := validateRequestTarget(config.Request); err != nil {
		return err
	}
	if err := validateScalar(config.Initial); err != nil {
		return err
	}
	if err := validateSource(config.Next, ValueString, ValueInteger); err != nil {
		return err
	}
	return validateOptionalSource(config.HasMore, ValueBoolean)
}

func validateOffset(config *OffsetConfig, itemsPath string) error {
	if config == nil {
		return invalid("offset strategy and non-negative start are required")
	}
	if config.Start < 0 {
		return invalid("offset strategy and non-negative start are required")
	}
	if err := validateRequestTarget(config.Request); err != nil {
		return err
	}
	if err := validateOffsetIncrement(config.Increment, config.PageSize, itemsPath); err != nil {
		return err
	}
	if err := validatePageSize(config.PageSize); err != nil {
		return err
	}
	if err := validateOffsetStop(config); err != nil {
		return err
	}
	return validateOffsetSources(config)
}

func validateOffsetStop(config *OffsetConfig) error {
	stopSources := []bool{config.NextOffset != nil, config.TotalItems != nil, config.HasMore != nil, config.StopOnShortPage}
	if !anyTrue(stopSources) {
		return invalid("offset strategy requires a stop signal")
	}
	if config.StopOnShortPage && config.PageSize == nil {
		return invalid("short-page stopping requires page_size")
	}
	return nil
}

func validateOffsetSources(config *OffsetConfig) error {
	if err := validateOptionalSource(config.NextOffset, ValueInteger); err != nil {
		return err
	}
	if err := validateOptionalSource(config.TotalItems, ValueInteger); err != nil {
		return err
	}
	return validateOptionalSource(config.HasMore, ValueBoolean)
}

func validatePageNumber(config *PageNumberConfig, _ string) error {
	if config == nil {
		return invalid("page_number requires positive start and increment")
	}
	if config.Start < 1 || config.Increment < 1 {
		return invalid("page_number requires positive start and increment")
	}
	if err := validateRequestTarget(config.Request); err != nil {
		return err
	}
	if err := validatePageSize(config.PageSize); err != nil {
		return err
	}
	if err := validatePageStop(config); err != nil {
		return err
	}
	if err := validateOptionalSource(config.TotalPages, ValueInteger); err != nil {
		return err
	}
	return validateOptionalSource(config.HasMore, ValueBoolean)
}

func validatePageStop(config *PageNumberConfig) error {
	if !anyTrue([]bool{config.TotalPages != nil, config.HasMore != nil, config.StopOnShortPage}) {
		return invalid("page_number strategy requires a stop signal")
	}
	if config.StopOnShortPage && config.PageSize == nil {
		return invalid("short-page stopping requires page_size")
	}
	return nil
}

func validateNextURL(config *NextURLConfig) error {
	if config == nil {
		return invalid("next_url strategy is required")
	}
	return validateSource(config.Next, ValueURL)
}

func validateOffsetIncrement(value OffsetIncrement, pageSize *PageSize, itemsPath string) error {
	switch value.Mode {
	case IncrementFixed:
		if value.Value < 1 {
			return invalid("fixed offset increment must be positive")
		}
	case IncrementItemsReturned:
		if value.Value != 0 || pageSize == nil || itemsPath == "" {
			return invalid("items_returned increment requires page_size and items_path")
		}
	default:
		return invalid("unsupported offset increment mode")
	}
	return nil
}

func validatePageSize(value *PageSize) error {
	if value == nil {
		return nil
	}
	if value.Value < 1 {
		return invalid("page_size value must be positive")
	}
	return validateRequestTarget(value.Target)
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
	if value.Type == ValueString && value.String != nil && value.Integer == nil {
		return nil
	}
	if value.Type == ValueInteger && value.Integer != nil && value.String == nil {
		return nil
	}
	return invalid("initial cursor must contain exactly one typed scalar")
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
	valid := value.Path == "" && validHeaderName(value.Name) && value.Relation == "next" && value.ValueType == ValueURL
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
