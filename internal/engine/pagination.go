package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var ErrPagination = errors.New("pagination execution failed")

// PaginationError deliberately carries only a stable code. Provider values and
// transport metadata must not escape through logs, traces, or Activity errors.
type PaginationError struct {
	Code string
}

func (e *PaginationError) Error() string { return "pagination execution failed: " + e.Code }
func (e *PaginationError) Unwrap() error { return ErrPagination }

func paginationError(code string) error { return &PaginationError{Code: code} }

func PaginationFailureCode(err error) string {
	var target *PaginationError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

type paginationState struct {
	value       any
	nextURL     *url.URL
	visited     map[string]struct{}
	itemsSeen   int64
	pagesSeen   int64
	bytesSeen   int64
	stopReason  string
	requestBase *url.URL
}

type paginationPage struct {
	body       bytes.Buffer
	headers    http.Header
	requestURL *url.URL
	maxBytes   int64
	headerKeys map[string]struct{}
}

type paginationAggregate struct {
	document any
	items    []any
}

func (p *paginationPage) Send(chunk []byte) error {
	if int64(p.body.Len()+len(chunk)) > p.maxBytes {
		return paginationError("max_bytes")
	}
	_, err := p.body.Write(chunk)
	return err
}

func (p *paginationPage) ResetForRetry() {
	p.body.Reset()
	p.headers = make(http.Header)
	p.requestURL = nil
}

func (p *paginationPage) CaptureResponseMetadata(headers http.Header, requestURL *url.URL) {
	for name := range p.headerKeys {
		if values := headers.Values(name); len(values) > 0 {
			p.headers[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	if requestURL != nil {
		clone := *requestURL
		p.requestURL = &clone
	}
}

func (d *Dispatcher) runPagination(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
) (status int, err error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.paginate")
	defer span.End()
	policy := (*paginationpolicy.Config)(obj.Pagination)
	if validationErr := paginationpolicy.Validate(policy); validationErr != nil {
		return 0, paginationError("invalid_config")
	}
	limits := paginationpolicy.EffectiveLimits(policy.Limits)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.MaxDurationMs)*time.Millisecond)
	defer cancel()
	state := newPaginationState(policy)
	defer func() {
		recordPaginationOutcome(ctx, span, state, policy.Type, err)
	}()
	return d.executePaginationPages(ctx, runCtx, srv, obj, cloneParams(params), credentials, bucketValues, stream, policy, limits, state)
}

func (d *Dispatcher) executePaginationPages(
	ctx, runCtx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	baseParams map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
	policy *paginationpolicy.Config,
	limits paginationpolicy.Limits,
	state *paginationState,
) (status int, err error) {
	aggregate := &paginationAggregate{}
	for {
		if err := checkPaginationContext(ctx, runCtx); err != nil {
			return status, err
		}
		if state.pagesSeen >= int64(limits.MaxPages) {
			return status, paginationError("max_pages")
		}

		pageObj, pageParams := paginationRequest(obj, baseParams, state)
		page := &paginationPage{
			maxBytes:   int64(limits.MaxBytes) - state.bytesSeen,
			headerKeys: paginationHeaderKeys(policy),
		}
		status, err = d.executeWithRetries(runCtx, srv, pageObj, pageParams, credentials, bucketValues, page)
		if err != nil {
			return status, paginationProviderError(ctx, err)
		}

		document, itemCount, decodeErr := decodePaginationPage(page.body.Bytes(), obj.Pagination.ItemsPath)
		if decodeErr != nil {
			return status, decodeErr
		}
		if state.itemsSeen+itemCount > int64(limits.MaxItems) {
			return status, paginationError("max_items")
		}
		if aggregateErr := aggregate.Add(document, obj.Pagination.ItemsPath); aggregateErr != nil {
			return status, aggregateErr
		}

		state.pagesSeen++
		state.itemsSeen += itemCount
		state.bytesSeen += int64(page.body.Len())
		observability.PaginationTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("type", string(obj.Pagination.Type))))
		continued, advanceErr := advancePagination(policy, state, document, page, itemCount)
		if advanceErr != nil {
			return status, advanceErr
		}
		if !continued {
			return status, sendPaginationAggregate(stream, aggregate, obj.Pagination.ItemsPath, limits.MaxBytes)
		}
	}
}

func (a *paginationAggregate) Add(document any, itemsPath string) error {
	items, found := valueAtPath(document, itemsPath)
	list, ok := items.([]any)
	if !found || !ok {
		return paginationError("response_invalid")
	}
	if a.document == nil {
		a.document = document
	}
	a.items = append(a.items, list...)
	return nil
}

func sendPaginationAggregate(stream ResponseStream, aggregate *paginationAggregate, itemsPath string, maxBytes int64) error {
	if aggregate == nil || aggregate.document == nil || !setValueAtPath(aggregate.document, itemsPath, aggregate.items) {
		return paginationError("response_invalid")
	}
	payload, err := json.Marshal(aggregate.document)
	if err != nil {
		return paginationError("response_invalid")
	}
	if int64(len(payload)) > maxBytes {
		return paginationError("max_bytes")
	}
	return stream.Send(payload)
}

func paginationProviderError(parent context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return paginationError("max_duration")
	}
	return err
}

func recordPaginationOutcome(ctx context.Context, span trace.Span, state *paginationState, policyType paginationpolicy.Type, err error) {
	summary := PaginationExecutionSummary{
		Type: string(policyType), PageCount: state.pagesSeen, ItemCount: state.itemsSeen,
		ByteCount: state.bytesSeen, StopReason: state.stopReason,
	}
	if summary.StopReason == "" && err != nil {
		summary.StopReason = PaginationFailureCode(err)
	}
	RecordPaginationSummary(ctx, summary)
	span.SetAttributes(
		attribute.String("pagination.type", summary.Type),
		attribute.Int64("pagination.page_count", summary.PageCount),
		attribute.Int64("pagination.item_count", summary.ItemCount),
		attribute.Int64("pagination.byte_count", summary.ByteCount),
		attribute.String("pagination.stop_reason", summary.StopReason),
	)
	if err != nil {
		span.SetStatus(codes.Error, summary.StopReason)
	}
}

func checkPaginationContext(parent, run context.Context) error {
	select {
	case <-run.Done():
		if parent.Err() != nil {
			return parent.Err()
		}
		return paginationError("max_duration")
	default:
		return nil
	}
}

func newPaginationState(config *paginationpolicy.Config) *paginationState {
	state := &paginationState{visited: make(map[string]struct{})}
	switch config.Type {
	case paginationpolicy.TypeCursor:
		if config.Cursor.Initial != nil {
			state.value = scalarValue(*config.Cursor.Initial)
			state.visited[scalarKey(state.value)] = struct{}{}
		}
	case paginationpolicy.TypeOffset:
		state.value = config.Offset.Start
		state.visited[scalarKey(state.value)] = struct{}{}
	case paginationpolicy.TypePageNumber:
		state.value = config.PageNumber.Start
		state.visited[scalarKey(state.value)] = struct{}{}
	}
	return state
}

func paginationRequest(obj *models.IntegrationObject, params map[string]any, state *paginationState) (*models.IntegrationObject, map[string]any) {
	pageObj := *obj
	pageObj.Parameters = append(models.Parameters(nil), obj.Parameters...)
	pageParams := cloneParams(params)
	if state.nextURL != nil {
		pageObj.Path = state.nextURL.String()
		pageParams = headerParamsOnly(pageParams, obj.Parameters)
	}
	switch obj.Pagination.Type {
	case paginationpolicy.TypeCursor:
		if state.value != nil {
			injectPaginationTarget(&pageObj, pageParams, obj.Pagination.Cursor.Request, state.value)
		}
	case paginationpolicy.TypeOffset:
		injectPaginationTarget(&pageObj, pageParams, obj.Pagination.Offset.Request, state.value)
		injectPageSize(&pageObj, pageParams, obj.Pagination.Offset.PageSize)
	case paginationpolicy.TypePageNumber:
		injectPaginationTarget(&pageObj, pageParams, obj.Pagination.PageNumber.Request, state.value)
		injectPageSize(&pageObj, pageParams, obj.Pagination.PageNumber.PageSize)
	}
	return &pageObj, pageParams
}

func injectPageSize(obj *models.IntegrationObject, params map[string]any, pageSize *paginationpolicy.PageSize) {
	if pageSize != nil {
		injectPaginationTarget(obj, params, pageSize.Target, pageSize.Value)
	}
}

func injectPaginationTarget(obj *models.IntegrationObject, params map[string]any, target paginationpolicy.RequestTarget, value any) {
	params[target.Name] = value
	for i := range obj.Parameters {
		if obj.Parameters[i].Name == target.Name {
			obj.Parameters[i].In = string(target.Location)
			return
		}
	}
	obj.Parameters = append(obj.Parameters, models.Parameter{Name: target.Name, In: string(target.Location)})
}

func cloneParams(params map[string]any) map[string]any {
	clone := make(map[string]any, len(params))
	for key, value := range params {
		clone[key] = value
	}
	return clone
}

func headerParamsOnly(params map[string]any, definitions models.Parameters) map[string]any {
	result := make(map[string]any)
	if headers, ok := params["_headers"]; ok {
		result["_headers"] = headers
	}
	for _, definition := range definitions {
		if definition.In == "header" {
			if value, ok := params[definition.Name]; ok {
				result[definition.Name] = value
			}
		}
	}
	return result
}

func decodePaginationPage(payload []byte, itemsPath string) (any, int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, 0, paginationError("response_invalid")
	}
	items, found := valueAtPath(document, itemsPath)
	if !found {
		return nil, 0, paginationError("response_invalid")
	}
	list, ok := items.([]any)
	if !ok {
		return nil, 0, paginationError("response_invalid")
	}
	return document, int64(len(list)), nil
}

func advancePagination(config *paginationpolicy.Config, state *paginationState, document any, page *paginationPage, itemCount int64) (bool, error) {
	switch config.Type {
	case paginationpolicy.TypeCursor:
		return advanceCursor(config.Cursor, state, document, page)
	case paginationpolicy.TypeOffset:
		return advanceOffset(config.Offset, state, document, page, itemCount)
	case paginationpolicy.TypePageNumber:
		return advancePageNumber(config.PageNumber, state, document, page, itemCount)
	case paginationpolicy.TypeNextURL:
		return advanceNextURL(config.NextURL, state, document, page)
	default:
		return false, paginationError("invalid_config")
	}
}

func advanceCursor(config *paginationpolicy.CursorConfig, state *paginationState, document any, page *paginationPage) (bool, error) {
	if stop, err := sourceSaysStop(config.HasMore, document, page); stop || err != nil {
		if stop {
			state.stopReason = "has_more_false"
		}
		return false, err
	}
	next, found, err := readSource(config.Next, document, page)
	if err != nil {
		return false, err
	}
	if !found || next == "" {
		state.stopReason = "missing_next"
		return false, nil
	}
	if err := rememberContinuation(state, next); err != nil {
		return false, err
	}
	state.value = next
	return true, nil
}

func advanceOffset(config *paginationpolicy.OffsetConfig, state *paginationState, document any, page *paginationPage, itemCount int64) (bool, error) {
	if done, reason, err := commonPageStop(config.HasMore, config.TotalItems, nil, config.PageSize, config.StopOnShortPage, state, document, page, itemCount); done || err != nil {
		state.stopReason = reason
		return false, err
	}
	current := state.value.(int64)
	next := current
	if config.NextOffset != nil {
		value, found, err := readSource(*config.NextOffset, document, page)
		if err != nil {
			return false, err
		}
		if !found {
			state.stopReason = "missing_next"
			return false, nil
		}
		next = value.(int64)
	} else if config.Increment.Mode == "fixed" {
		next += config.Increment.Value
	} else {
		next += itemCount
	}
	if err := rememberContinuation(state, next); err != nil {
		return false, err
	}
	state.value = next
	return true, nil
}

func advancePageNumber(config *paginationpolicy.PageNumberConfig, state *paginationState, document any, page *paginationPage, itemCount int64) (bool, error) {
	if done, reason, err := commonPageStop(config.HasMore, nil, config.TotalPages, config.PageSize, config.StopOnShortPage, state, document, page, itemCount); done || err != nil {
		state.stopReason = reason
		return false, err
	}
	next := state.value.(int64) + config.Increment
	if err := rememberContinuation(state, next); err != nil {
		return false, err
	}
	state.value = next
	return true, nil
}

func commonPageStop(hasMore, totalItems, totalPages *paginationpolicy.ValueSource, pageSize *paginationpolicy.PageSize, shortPage bool, state *paginationState, document any, page *paginationPage, itemCount int64) (bool, string, error) {
	if decision := booleanStopDecision(hasMore, document, page); decision.done {
		return decision.stop, "has_more_false", decision.err
	}
	if decision := totalStopDecision(totalItems, state.itemsSeen, document, page); decision.done {
		return decision.stop, "total_reached", decision.err
	}
	if decision := totalStopDecision(totalPages, state.value.(int64), document, page); decision.done {
		return decision.stop, "total_pages_reached", decision.err
	}
	return itemCountStop(pageSize, shortPage, itemCount)
}

type paginationStopDecision struct {
	done bool
	stop bool
	err  error
}

func booleanStopDecision(source *paginationpolicy.ValueSource, document any, page *paginationPage) paginationStopDecision {
	stop, err := sourceSaysStop(source, document, page)
	return paginationStopDecision{done: stop || err != nil, stop: stop, err: err}
}

func totalStopDecision(source *paginationpolicy.ValueSource, current int64, document any, page *paginationPage) paginationStopDecision {
	stop, err := reachedTotal(source, current, document, page)
	return paginationStopDecision{done: stop || err != nil, stop: stop, err: err}
}

func itemCountStop(pageSize *paginationpolicy.PageSize, shortPage bool, itemCount int64) (bool, string, error) {
	if itemCount == 0 {
		return true, "empty_page", nil
	}
	if shortPage && pageSize != nil && itemCount < pageSize.Value {
		return true, "short_page", nil
	}
	return false, "", nil
}

func sourceSaysStop(source *paginationpolicy.ValueSource, document any, page *paginationPage) (bool, error) {
	if source == nil {
		return false, nil
	}
	value, found, err := readSource(*source, document, page)
	if err != nil {
		return false, err
	}
	if !found {
		return false, paginationError("response_invalid")
	}
	return !value.(bool), nil
}

func reachedTotal(source *paginationpolicy.ValueSource, current int64, document any, page *paginationPage) (bool, error) {
	if source == nil {
		return false, nil
	}
	value, found, err := readSource(*source, document, page)
	if err != nil {
		return false, err
	}
	if !found {
		return false, paginationError("response_invalid")
	}
	return current >= value.(int64), nil
}

func advanceNextURL(config *paginationpolicy.NextURLConfig, state *paginationState, document any, page *paginationPage) (bool, error) {
	next, found, err := readSource(config.Next, document, page)
	if err != nil {
		return false, err
	}
	if !found || next == "" {
		state.stopReason = "missing_next"
		return false, nil
	}
	resolved, err := trustedNextURL(page.requestURL, next.(string))
	if err != nil {
		return false, err
	}
	key := resolved.String()
	if _, exists := state.visited[key]; exists {
		return false, paginationError("cycle")
	}
	state.visited[key] = struct{}{}
	state.nextURL = resolved
	return true, nil
}

func readSource(source paginationpolicy.ValueSource, document any, page *paginationPage) (any, bool, error) {
	var value any
	var found bool
	switch source.Location {
	case "body":
		value, found = valueAtPath(document, source.Path)
	case "header":
		value, found = headerValue(page.headers, source.Name)
	case "link":
		value, found = linkValue(page.headers.Values(source.Name), source.Relation)
	default:
		return nil, false, paginationError("invalid_config")
	}
	if !found || value == nil {
		return nil, false, nil
	}
	converted, err := convertSourceValue(value, string(source.ValueType))
	if err != nil {
		return nil, false, err
	}
	return converted, true, nil
}

func valueAtPath(document any, path string) (any, bool) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "$")
	path = strings.TrimPrefix(path, ".")
	current := document
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func setValueAtPath(document any, path string, value any) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(path), "$"), "."), ".")
	current, ok := document.(map[string]any)
	if !ok || len(parts) == 0 {
		return false
	}
	for _, part := range parts[:len(parts)-1] {
		next, exists := current[part]
		if !exists {
			return false
		}
		current, ok = next.(map[string]any)
		if !ok {
			return false
		}
	}
	leaf := parts[len(parts)-1]
	if _, exists := current[leaf]; !exists {
		return false
	}
	current[leaf] = value
	return true
}

func headerValue(headers http.Header, name string) (any, bool) {
	value := headers.Get(name)
	return value, value != ""
}

func linkValue(values []string, relation string) (any, bool) {
	for _, header := range values {
		for _, entry := range splitLinkHeader(header) {
			segments := strings.Split(entry, ";")
			if len(segments) < 2 {
				continue
			}
			target := strings.Trim(strings.TrimSpace(segments[0]), "<>")
			for _, parameter := range segments[1:] {
				parts := strings.SplitN(strings.TrimSpace(parameter), "=", 2)
				if len(parts) == 2 && strings.EqualFold(parts[0], "rel") {
					for _, rel := range strings.Fields(strings.Trim(parts[1], `"`)) {
						if rel == relation {
							return target, true
						}
					}
				}
			}
		}
	}
	return nil, false
}

// splitLinkHeader respects URI references and quoted extension parameters;
// commas inside either are data rather than RFC 8288 link-value separators.
func splitLinkHeader(header string) []string {
	var entries []string
	start := 0
	state := linkSplitState{}
	for index, char := range header {
		if state.separator(char) {
			entries = append(entries, header[start:index])
			start = index + 1
		}
	}
	return append(entries, header[start:])
}

type linkSplitState struct {
	angleDepth int
	quoted     bool
	escaped    bool
}

func (s *linkSplitState) separator(char rune) bool {
	if s.escaped {
		s.escaped = false
		return false
	}
	if s.quoted {
		if char == '\\' {
			s.escaped = true
		}
		if char == '"' {
			s.quoted = false
		}
		return false
	}
	switch char {
	case '"':
		s.quoted = true
	case '<':
		s.angleDepth++
	case '>':
		if s.angleDepth > 0 {
			s.angleDepth--
		}
	case ',':
		return s.angleDepth == 0
	}
	return false
}

func convertSourceValue(value any, valueType string) (any, error) {
	switch valueType {
	case "string", "url":
		return convertStringSource(value)
	case "boolean":
		if typed, ok := value.(bool); ok {
			return typed, nil
		}
	case "integer":
		return convertIntegerSource(value)
	}
	return nil, paginationError("response_invalid")
}

func convertStringSource(value any) (any, error) {
	typed, ok := value.(string)
	if !ok {
		return nil, paginationError("response_invalid")
	}
	if len(typed) > 4096 {
		return nil, paginationError("continuation_invalid")
	}
	return typed, nil
}

func convertIntegerSource(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, nil
		}
	case string:
		if integer, err := strconv.ParseInt(typed, 10, 64); err == nil {
			return integer, nil
		}
	}
	return nil, paginationError("response_invalid")
}

func scalarValue(value paginationpolicy.Scalar) any {
	if value.Type == "integer" {
		return *value.Integer
	}
	return *value.String
}

func scalarKey(value any) string {
	switch typed := value.(type) {
	case int64:
		return "i:" + strconv.FormatInt(typed, 10)
	default:
		return "s:" + fmt.Sprint(typed)
	}
}

func rememberContinuation(state *paginationState, value any) error {
	key := scalarKey(value)
	if _, exists := state.visited[key]; exists {
		return paginationError("cycle")
	}
	state.visited[key] = struct{}{}
	return nil
}

func paginationHeaderKeys(config *paginationpolicy.Config) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, source := range paginationSources(config) {
		if source != nil && (source.Location == "header" || source.Location == "link") {
			keys[http.CanonicalHeaderKey(source.Name)] = struct{}{}
		}
	}
	return keys
}

func paginationSources(config *paginationpolicy.Config) []*paginationpolicy.ValueSource {
	switch config.Type {
	case paginationpolicy.TypeCursor:
		return []*paginationpolicy.ValueSource{&config.Cursor.Next, config.Cursor.HasMore}
	case paginationpolicy.TypeOffset:
		return []*paginationpolicy.ValueSource{config.Offset.NextOffset, config.Offset.TotalItems, config.Offset.HasMore}
	case paginationpolicy.TypePageNumber:
		return []*paginationpolicy.ValueSource{config.PageNumber.TotalPages, config.PageNumber.HasMore}
	case paginationpolicy.TypeNextURL:
		return []*paginationpolicy.ValueSource{&config.NextURL.Next}
	default:
		return nil
	}
}

func trustedNextURL(previous *url.URL, raw string) (*url.URL, error) {
	if previous == nil || previous.User != nil || len(raw) > 4096 {
		return nil, paginationError("untrusted_next_url")
	}
	next, err := url.Parse(raw)
	if err != nil || next.User != nil || next.Fragment != "" {
		return nil, paginationError("untrusted_next_url")
	}
	resolved := previous.ResolveReference(next)
	if !sameOrigin(previous, resolved) {
		return nil, paginationError("untrusted_next_url")
	}
	return resolved, nil
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || (right.Scheme != "http" && right.Scheme != "https") {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if value.Port() != "" {
		return value.Port()
	}
	if value.Scheme == "https" {
		return "443"
	}
	return "80"
}
