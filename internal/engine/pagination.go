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

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"go.opentelemetry.io/otel"
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

type paginationPage struct {
	body        bytes.Buffer
	headers     http.Header
	requestURL  *url.URL
	maxBytes    int64
	headerKeys  map[string]struct{}
	status      int
	mediaFamily string
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

// ResetForRetry discards every response fact from an attempt that retry policy
// rejected, preventing its status or body from leaking into the final result.
func (p *paginationPage) ResetForRetry() {
	p.body.Reset()
	p.headers = make(http.Header)
	p.requestURL = nil
	p.status = 0
	p.mediaFamily = ""
}

// SendResponseContract captures the provider status and reviewed media family
// while pagination buffers a page; failed responses are later forwarded intact.
func (p *paginationPage) SendResponseContract(status int, mediaFamily string) error {
	p.status = status
	p.mediaFamily = boundedResponseContractFamily(mediaFamily)
	return nil
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
	return d.runPaginationV3(ctx, span, srv, obj, params, credentials, bucketValues, stream, policy)
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
	document, ok := aggregate.result(itemsPath)
	if !ok {
		return paginationError("response_invalid")
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return paginationError("response_invalid")
	}
	if int64(len(payload)) > maxBytes {
		return paginationError("max_bytes")
	}
	return stream.Send(payload)
}

func sendPaginationResult(stream ResponseStream, status int, aggregate *paginationAggregate, itemsPath string, maxBytes int64) error {
	if err := SendResponseContract(stream, status, "json"); err != nil {
		return err
	}
	return sendPaginationAggregate(stream, aggregate, itemsPath, maxBytes)
}

// paginationResponseIsSuccessful centralizes the boundary between documents
// pagination may interpret and provider outcomes it must preserve unchanged.
func paginationResponseIsSuccessful(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// sendPaginationProviderResponse preserves non-success provider semantics
// instead of interpreting an error document as a successful pagination page.
func sendPaginationProviderResponse(stream ResponseStream, status int, page *paginationPage) error {
	family := "unknown"
	if page != nil && page.status == status {
		family = page.mediaFamily
	}
	if err := SendResponseContract(stream, status, family); err != nil {
		return err
	}
	if page == nil || page.body.Len() == 0 {
		return nil
	}
	return stream.Send(page.body.Bytes())
}

func (a *paginationAggregate) result(itemsPath string) (any, bool) {
	if a == nil || a.document == nil {
		return nil, false
	}
	// A root array has no parent object whose field can be replaced after
	// aggregation, so the accumulated items are the response document.
	if isRootBodyPath(itemsPath) {
		return a.items, true
	}
	if !setValueAtPath(a.document, itemsPath, a.items) {
		return nil, false
	}
	return a.document, true
}

func paginationProviderError(parent context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return paginationError("max_duration")
	}
	return err
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

func valueAtPath(document any, path string) (any, bool) {
	if isRootBodyPath(path) {
		return document, true
	}
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

func isRootBodyPath(path string) bool {
	return strings.TrimSpace(path) == "$"
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

func scalarKey(value any) string {
	switch typed := value.(type) {
	case int64:
		return "i:" + strconv.FormatInt(typed, 10)
	default:
		return "s:" + fmt.Sprint(typed)
	}
}
