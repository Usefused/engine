package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	neturl "net/url"
	"regexp"

	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Dispatcher coordinates the execution of vendor API calls.
type Dispatcher struct {
	client *http.Client
}

// NewDispatcher creates a new execution dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		client: newProviderHTTPClient(),
	}
}

func newProviderHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: newProviderHTTPTransport(nil),
	}
}

// newProviderHTTPTransport centralizes provider transport tuning so the normal
// shared client and scoped mTLS clients keep the same timeout/keepalive posture.
func newProviderHTTPTransport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
}

func requestWithProviderHTTPTrace(ctx context.Context, req *http.Request, srv *models.Service, obj *models.IntegrationObject, span trace.Span) *http.Request {
	clientTrace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			idleMs := float64(info.IdleTime.Microseconds()) / 1000
			if span.IsRecording() {
				span.SetAttributes(
					attribute.Bool("http.connection.reused", info.Reused),
					attribute.Bool("http.connection.was_idle", info.WasIdle),
					attribute.Float64("http.connection.idle_time_ms", idleMs),
				)
			}
			slog.InfoContext(ctx, "Provider HTTP connection selected",
				slog.String("service", srv.Name),
				slog.String("endpoint", obj.Name),
				slog.String("method", obj.Method),
				slog.String("host", req.URL.Host),
				slog.Bool("reused", info.Reused),
				slog.Bool("was_idle", info.WasIdle),
				slog.Float64("idle_time_ms", idleMs),
			)
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), clientTrace))
}

// ResponseStream and its buffering implementation live in response_stream.go.

// ExecuteStream performs the vendor call, handling SSE streams, pagination, and retries.
func (d *Dispatcher) ExecuteStream(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
) (int, error) {
	if obj.Method == "SOAP" {
		return d.executeSOAP(ctx, srv, obj, params, credentials, bucketValues, stream)
	}
	if obj.IsSSE {
		return d.executeSSE(ctx, srv, obj, params, credentials, bucketValues, stream)
	}
	if obj.Pagination != nil {
		return d.executePaginated(ctx, srv, obj, params, credentials, bucketValues, stream)
	}
	return d.executeWithRetries(ctx, srv, obj, params, credentials, bucketValues, stream)
}

// executeSSE makes an SSE request to the vendor, parses each event, and
// streams the parsed data payload (without the `data:` prefix) as individual
// chunks. The function stops when the stream is closed or the [DONE] sentinel
// is received (OpenAI-style). Each event is forwarded as a chunk so the SDK
// receives clean data rather than raw SSE wire format.
func (d *Dispatcher) executeSSE(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
) (int, error) {
	reqURL, headers, bodyReader, err := prepareRequestParts(srv, obj, params, bucketValues)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare SSE request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, obj.Method, reqURL, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("failed to build SSE request: %w", err)
	}
	req = requestWithProviderHTTPTrace(ctx, req, srv, obj, trace.SpanFromContext(ctx))

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range srv.DefaultHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	// Signal to the vendor that we want an SSE stream.
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	applyAuth(req, srv.AuthConfigs, credentials)

	start := time.Now()
	client, err := d.providerClientForAuth(srv.AuthConfigs, credentials)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)

	duration := float64(time.Since(start).Milliseconds())
	observability.RequestsDuration.Record(ctx, duration)

	if err != nil {
		observability.RequestsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("service", srv.Name),
			attribute.String("endpoint", obj.Name),
			attribute.String("status", "error"),
		))
		return 0, fmt.Errorf("network request failed: %w", err)
	}
	defer resp.Body.Close()

	observability.RequestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", srv.Name),
		attribute.String("endpoint", obj.Name),
		attribute.Int("status_code", resp.StatusCode),
	))

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("provider returned status %d: %s", resp.StatusCode, string(body))
		return resp.StatusCode, err
	}

	if err := parseSSEEvents(resp.Body, stream); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// parseSSEEvents reads an SSE stream line-by-line and sends each complete event
// to the stream. Lines starting with "data:" are accumulated into an event; a
// blank line signals the end of one event. Empty data and [DONE] sentinels are
// skipped. All other SSE fields (event:, id:, retry:, comments) are ignored
// because the Engine's wire contract uses only the data payload.
func parseSSEEvents(body io.Reader, stream ResponseStream) error {
	scanner := bufio.NewScanner(body)
	var dataBuf strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "data:"):
			// done=true on the [DONE] sentinel — stream is complete.
			if done := appendSSEData(line, &dataBuf); done {
				return nil
			}
		case line == "":
			// Blank line = end of one SSE event; flush what we have.
			if err := flushSSEEvent(&dataBuf, stream); err != nil {
				return err
			}
		default:
			// Ignore event:, id:, retry:, and comment lines (":").
			continue
		}
	}

	// Flush any trailing event not terminated by a blank line.
	if err := flushSSEEvent(&dataBuf, stream); err != nil {
		return err
	}
	return scanner.Err()
}

// appendSSEData handles one "data:" line. It returns done=true on the [DONE]
// sentinel; otherwise it appends the (non-empty) payload to buf.
func appendSSEData(line string, buf *strings.Builder) (done bool) {
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return true
	}
	if payload == "" {
		return false
	}
	if buf.Len() > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString(payload)
	return false
}

// flushSSEEvent sends the accumulated event (if any) and resets the buffer. Used
// both on the blank-line boundary and for a trailing unterminated event.
func flushSSEEvent(buf *strings.Builder, stream ResponseStream) error {
	if buf.Len() == 0 {
		return nil
	}
	if err := stream.Send([]byte(buf.String())); err != nil {
		return fmt.Errorf("failed to stream SSE event: %w", err)
	}
	buf.Reset()
	return nil
}

func (d *Dispatcher) executePaginated(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
) (int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.paginate")
	defer span.End()

	// Copy params to avoid mutating the original map across pages.
	p := make(map[string]any)
	for k, v := range params {
		p[k] = v
	}

	var finalStatus int

	// Cap at 100 pages to prevent runaway loops.
	for page := 0; page < 100; page++ {
		pageStream := NewBufferStream()
		status, err := d.executeWithRetries(ctx, srv, obj, p, credentials, bucketValues, pageStream)
		finalStatus = status

		observability.PaginationTotal.Add(ctx, 1)
		span.SetAttributes(attribute.Int("page_count", page+1))

		if err != nil {
			return status, err
		}

		// Stream the chunk back.
		if sendErr := stream.Send(pageStream.Bytes()); sendErr != nil {
			return status, sendErr
		}

		nextToken := extractNextToken(pageStream.Bytes(), obj.Pagination.ResponsePath)
		// Break if no next token exists.
		if nextToken == "" {
			break
		}

		p[obj.Pagination.RequestParam] = nextToken
	}

	return finalStatus, nil
}

func extractNextToken(payload []byte, path string) string {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return ""
	}

	parts := strings.Split(path, ".")
	var current any = data

	// Traverse JSON map hierarchy to find the token.
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[part]
	}

	if str, ok := current.(string); ok {
		return str
	}
	return ""
}

func (d *Dispatcher) executeWithRetries(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
) (int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.retry")
	defer span.End()

	// Strictly config-driven: no hardcoded retry count or backoff. If the
	// service has no RetryConfig and the caller passed no RetryOverride,
	// policy is nil and the loop below runs exactly once. See
	// resolveRetryPolicy for the full precedence rules, and
	// resolveEffectiveMaxRetries for the HTTP-method safety gate on top of it.
	policy := resolveRetryPolicy(srv.RetryConfig, RetryOverrideFromContext(ctx))
	maxRetries := resolveEffectiveMaxRetries(ctx, span, policy, obj.Method)

	var lastErr error
	var lastStatus int

	for attempt := 0; attempt <= maxRetries; attempt++ {
		RecordExecutionCount(ctx, "provider_attempt_count", int64(attempt+1))
		if attempt > 0 {
			observability.RetriesTotal.Add(ctx, 1)
			span.SetAttributes(attribute.Int("retry_count", attempt))
		}

		status, err := d.executeOnce(ctx, srv, obj, params, credentials, bucketValues, stream, attempt)

		// If success or non-retryable error, return immediately.
		if err == nil && status < 500 {
			return status, nil
		}

		lastErr = err
		lastStatus = status

		if attempt < maxRetries {
			if delay := retrypolicy.BackoffDuration(policy.Strategy, policy.BackoffMs, attempt); delay > 0 {
				time.Sleep(delay)
			}
		}
	}

	return lastStatus, normalizeError(lastErr, lastStatus)
}

func (d *Dispatcher) executeOnce(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
	attempt int,
) (int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.vendor_call", trace.WithAttributes(
		attribute.String("http.method", obj.Method),
		attribute.String("peer.service", srv.Name),
	))
	defer span.End()

	prepareStarted := time.Now()
	reqURL, headers, bodyReader, err := prepareRequestParts(srv, obj, params, bucketValues)
	if err != nil {
		MeasureExecutionTiming(ctx, "provider_request_prepare", prepareStarted)
		span.SetStatus(codes.Error, err.Error())
		return 0, fmt.Errorf("failed to prepare request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, obj.Method, reqURL, bodyReader)
	if err != nil {
		MeasureExecutionTiming(ctx, "provider_request_prepare", prepareStarted)
		return 0, fmt.Errorf("failed to build request: %w", err)
	}
	req = requestWithProviderHTTPTrace(ctx, req, srv, obj, span)

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range srv.DefaultHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	applyAuth(req, srv.AuthConfigs, credentials)
	MeasureExecutionTiming(ctx, "provider_request_prepare", prepareStarted)

	providerStarted := time.Now()
	client, err := d.providerClientForAuth(srv.AuthConfigs, credentials)
	if err != nil {
		MeasureExecutionTiming(ctx, "provider_total", providerStarted)
		return 0, err
	}
	resp, err := client.Do(req)
	MeasureExecutionTiming(ctx, "provider_time_to_headers", providerStarted)
	if err != nil {
		MeasureExecutionTiming(ctx, "provider_total", providerStarted)
		return 0, fmt.Errorf("network request failed: %w", err)
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	status, err := streamResponseBody(ctx, resp.Body, resp.StatusCode, stream)
	MeasureExecutionTiming(ctx, "provider_total", providerStarted)
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		span.SetAttributes(timings.Attributes()...)
	}
	return status, err
}

func streamResponseBody(ctx context.Context, body io.Reader, status int, stream ResponseStream) (int, error) {
	bodyStarted := time.Now()
	var sendDuration time.Duration
	defer func() {
		bodyDuration := time.Since(bodyStarted)
		RecordExecutionTiming(ctx, "provider_body_stream", bodyDuration)
		RecordExecutionTiming(ctx, "engine_stream_send", sendDuration)
		if bodyDuration > sendDuration {
			RecordExecutionTiming(ctx, "provider_body_read", bodyDuration-sendDuration)
		}
	}()

	buffer := make([]byte, 4096)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			sendStarted := time.Now()
			if sendErr := stream.Send(buffer[:n]); sendErr != nil {
				sendDuration += time.Since(sendStarted)
				return status, fmt.Errorf("failed to stream response chunk: %w", sendErr)
			}
			sendDuration += time.Since(sendStarted)
		}
		if err == io.EOF {
			return status, nil
		}
		if err != nil {
			return status, fmt.Errorf("error reading response body: %w", err)
		}
	}
}

func normalizeError(err error, status int) error {
	if err == nil {
		if status >= 400 && status < 500 {
			return fmt.Errorf("client error %d: bad request or unauthorized", status)
		}
		if status >= 500 {
			return fmt.Errorf("server error %d: upstream provider failed", status)
		}
		return nil
	}
	return fmt.Errorf("execution failed after retries: %w", err)
}

func prepareRequestParts(srv *models.Service, obj *models.IntegrationObject, params map[string]any, bucketValues []store.BucketValue) (string, map[string]string, io.Reader, error) {
	if err := validateResolvedBindingHeaders(bucketValues); err != nil {
		return "", nil, nil, err
	}
	queryParams := neturl.Values{}
	headerParams := make(map[string]string)
	bodyParams := make(map[string]any)

	baseURL := bindingBaseURL(srv.BaseURL, bucketValues)
	reqURL := buildBaseURL(obj.Path, baseURL)
	applyForcedPathBindings(&reqURL, bucketValues)
	extractCustomHeaders(params, headerParams)
	reqURL = mapParameters(reqURL, obj, params, queryParams, headerParams, bodyParams)
	applyDefaultBindings(&reqURL, queryParams, headerParams, bodyParams, bucketValues)
	applyForcedBindings(&reqURL, queryParams, headerParams, bodyParams, bucketValues)

	slog.Info("DISPATCH DEBUG", slog.Any("headers", headerParams), slog.Any("bucketValues", bucketValues))
	reqURL = appendQueryParams(reqURL, queryParams)

	bodyReader, err := buildRequestBody(obj, srv, headerParams, bodyParams)
	if err != nil {
		return "", nil, nil, err
	}

	return reqURL, headerParams, bodyReader, nil
}

// validateResolvedBindingHeaders rechecks stored names at the final trust
// boundary so corrupted rows cannot smuggle protected transport headers.
func validateResolvedBindingHeaders(bindings []store.BucketValue) error {
	for _, binding := range bindings {
		if strings.EqualFold(binding.Location, "header") {
			if err := connectionprofile.ValidateResolvedHeader(binding.KeyName, binding.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// bindingBaseURL selects only the trusted resource-derived forced base URL;
// ordinary literals cannot redirect provider traffic.
func bindingBaseURL(serviceDefault string, bindings []store.BucketValue) string {
	baseURL := serviceDefault
	for _, binding := range bindings {
		// Host selection is privileged: only a URL resolved from a validated
		// connection resource may replace the service's versioned static host.
		if binding.Location != "base_url" || binding.SourceKind != "connection_resource" {
			continue
		}
		if binding.Mode == "force" || (binding.Mode == "default" && baseURL == "") {
			baseURL = binding.Value
		}
	}
	return baseURL
}

// applyDefaultBindings fills absent values first so caller parameters retain
// precedence over non-forced workspace defaults.
func applyDefaultBindings(reqURL *string, query neturl.Values, headers map[string]string, body map[string]any, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if normalizedBindingMode(binding.Mode) != "default" || binding.Location == "base_url" {
			continue
		}
		applyBinding(reqURL, query, headers, body, binding, false)
	}
}

// applyForcedBindings runs after caller mapping because tenant-routing values
// must not be overridden by SDK input.
func applyForcedBindings(reqURL *string, query neturl.Values, headers map[string]string, body map[string]any, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if normalizedBindingMode(binding.Mode) != "force" || binding.Location == "base_url" || binding.Location == "path" {
			continue
		}
		applyBinding(reqURL, query, headers, body, binding, true)
	}
}

// applyForcedPathBindings runs after path parameters are rendered so selected
// resource context wins over a conflicting caller path value.
func applyForcedPathBindings(reqURL *string, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if normalizedBindingMode(binding.Mode) == "force" && binding.Location == "path" {
			applyPathBinding(reqURL, binding.KeyName, binding.Value, true)
		}
	}
}

// normalizedBindingMode keeps migrated rows backward compatible by treating an
// omitted mode as a non-destructive default.
func normalizedBindingMode(mode string) string {
	// Legacy bucket values predate modes and behaved as forced values.
	if mode == "" {
		return "force"
	}
	return strings.ToLower(mode)
}

// applyBinding dispatches a validated binding to its transport owner instead of
// letting target-specific rules leak into resolution or storage.
func applyBinding(reqURL *string, query neturl.Values, headers map[string]string, body map[string]any, binding store.BucketValue, force bool) {
	switch strings.ToLower(binding.Location) {
	case "header":
		applyHeaderBinding(headers, binding.KeyName, binding.Value, force)
	case "query":
		applyQueryBinding(query, binding.KeyName, binding.Value, force)
	case "path":
		applyPathBinding(reqURL, binding.KeyName, binding.Value, force)
	case "body":
		if force || body[binding.KeyName] == nil {
			body[binding.KeyName] = binding.Value
		}
	}
}

// applyHeaderBinding compares names case-insensitively because HTTP header
// casing must not provide an override bypass.
func applyHeaderBinding(headers map[string]string, name, value string, force bool) {
	existing := matchingHeaderKey(headers, name)
	if existing != "" && !force {
		return
	}
	if existing != "" && existing != name {
		delete(headers, existing)
	}
	headers[name] = value
}

// matchingHeaderKey returns the caller's original casing while enforcing HTTP's
// case-insensitive identity rules.
func matchingHeaderKey(headers map[string]string, name string) string {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return key
		}
	}
	return ""
}

// applyQueryBinding preserves explicit query input for defaults and replaces it
// only when the validated profile marks tenant context as forced.
func applyQueryBinding(query neturl.Values, name, value string, force bool) {
	if query.Has(name) && !force {
		return
	}
	// Set gives forced tenant context replacement semantics and prevents the
	// provider from receiving two conflicting account selectors.
	query.Set(name, value)
}

// applyPathBinding substitutes an encoded segment and leaves unmatched optional
// defaults alone, avoiding accidental whole-URL string replacement.
func applyPathBinding(reqURL *string, name, value string, force bool) {
	placeholder := regexp.MustCompile(`(?i)\{` + regexp.QuoteMeta(name) + `\}`)
	if !force && !placeholder.MatchString(*reqURL) {
		return
	}
	if placeholder.MatchString(*reqURL) {
		*reqURL = placeholder.ReplaceAllString(*reqURL, neturl.PathEscape(value))
	}
}

func buildBaseURL(reqURL, baseURL string) string {
	// If the request URL is not fully qualified and a base URL exists, prepend it
	if !strings.HasPrefix(reqURL, "http") && baseURL != "" {
		// Ensure we don't double-slash or miss a slash when joining base URL and path
		if !strings.HasPrefix(reqURL, "/") && !strings.HasSuffix(baseURL, "/") {
			return baseURL + "/" + reqURL
		}
		return baseURL + reqURL
	}
	return reqURL
}

func extractCustomHeaders(params map[string]any, headerParams map[string]string) {
	// Check if there is a custom _headers map provided in the parameters
	if headersMap, ok := params["_headers"].(map[string]any); ok {
		// Iterate over custom headers and add them to header parameters
		for k, v := range headersMap {
			headerParams[k] = fmt.Sprintf("%v", v)
		}
		// Remove _headers from params so it doesn't leak into the request body
		delete(params, "_headers")
	}
}

func mapParameters(reqURL string, obj *models.IntegrationObject, params map[string]any, queryParams neturl.Values, headerParams map[string]string, bodyParams map[string]any) string {
	paramLocations := make(map[string]string)
	// Build a lookup map of parameter names to their intended location (path, query, header)
	for _, p := range obj.Parameters {
		paramLocations[p.Name] = p.In
	}

	re := regexp.MustCompile(`(?i)\{([^{}]+)\}`)
	matches := re.FindAllStringSubmatch(reqURL, -1)

	// Iterate through all provided parameters and map them to the correct location
	for k, v := range params {
		valStr := fmt.Sprintf("%v", v)
		in := determineParamLocation(k, paramLocations, matches)
		reqURL = applyParam(in, k, v, valStr, reqURL, obj.Method, queryParams, headerParams, bodyParams)
	}
	return reqURL
}

func determineParamLocation(key string, paramLocations map[string]string, pathMatches [][]string) string {
	// Check if the parameter location is explicitly defined in the object's parameters
	if in, ok := paramLocations[key]; ok {
		return in
	}

	// Fallback: Look for case-insensitive path matches if it's missing from definition
	for _, match := range pathMatches {
		// If we find a match in the path template, mark it as a path parameter
		if len(match) > 1 && strings.EqualFold(match[1], key) {
			return "path"
		}
	}
	return ""
}

func applyParam(in, key string, val any, valStr, reqURL, method string, queryParams neturl.Values, headerParams map[string]string, bodyParams map[string]any) string {
	// Route the parameter to the appropriate location
	switch in {
	case "path":
		// Replace the path variable placeholder with the URL-encoded value
		encodedVal := neturl.PathEscape(valStr)
		return regexp.MustCompile(fmt.Sprintf(`(?i)\{%s\}`, regexp.QuoteMeta(key))).ReplaceAllString(reqURL, encodedVal)
	case "query":
		// SDK parameters replace defaults at their declared scalar target.
		queryParams.Set(key, valStr)
	case "header":
		// Add the parameter to the request headers
		headerParams[key] = valStr
	default:
		// If no location is known, fallback to query for GET/DELETE or body for others
		if method == http.MethodGet || method == http.MethodDelete {
			queryParams.Set(key, valStr)
		} else {
			bodyParams[key] = val
		}
	}
	return reqURL
}

func appendQueryParams(reqURL string, queryParams neturl.Values) string {
	// If there are any query parameters, encode and append them to the URL
	if len(queryParams) > 0 {
		// Use '&' if the URL already has a query string, otherwise '?'
		if strings.Contains(reqURL, "?") {
			return reqURL + "&" + queryParams.Encode()
		}
		return reqURL + "?" + queryParams.Encode()
	}
	return reqURL
}

func buildRequestBody(obj *models.IntegrationObject, srv *models.Service, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	// GET and DELETE requests should not have a request body
	if obj.Method == http.MethodGet || obj.Method == http.MethodDelete {
		return nil, nil
	}

	contentType := resolveContentType(obj, srv, headerParams)

	// If the resolved content type is urlencoded, construct a form
	if strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		// Only build the form body if there are actual parameters
		if len(bodyParams) > 0 {
			formValues := neturl.Values{}
			// Add each parameter to the form values
			for k, v := range bodyParams {
				formValues.Add(k, fmt.Sprintf("%v", v))
			}
			headerParams["Content-Type"] = "application/x-www-form-urlencoded"
			return strings.NewReader(formValues.Encode()), nil
		}
		return nil, nil
	}

	// For JSON content type, marshal the body parameters
	bodyBytes, err := json.Marshal(bodyParams)
	// If JSON marshaling fails, bubble up the error
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON body: %w", err)
	}
	headerParams["Content-Type"] = "application/json"
	return bytes.NewReader(bodyBytes), nil
}

func resolveContentType(obj *models.IntegrationObject, srv *models.Service, headerParams map[string]string) string {
	// Priority 1: User injected header (capitalized)
	if ct, ok := headerParams["Content-Type"]; ok {
		return ct
	}
	// Priority 1: User injected header (lowercase)
	if ct, ok := headerParams["content-type"]; ok {
		return ct
	}
	// Priority 2: Service default headers (capitalized)
	if ct, ok := srv.DefaultHeaders["Content-Type"]; ok {
		return ct
	}
	// Priority 2: Service default headers (lowercase)
	if ct, ok := srv.DefaultHeaders["content-type"]; ok {
		return ct
	}
	// Priority 3: Endpoint encoding override
	if obj.Encoding != "" {
		return obj.Encoding
	}
	// Fallback default
	return "application/json"
}

func applyAuth(req *http.Request, auths models.AuthConfigs, credentials map[string]any) {
	if len(auths) == 0 {
		return
	}
	auth := auths[0]

	switch auth.Type {
	case "http":
		applyHTTPAuth(req, auth, credentials)
	case "oauth2", "openIdConnect", "oidc":
		applyOAuth(req, auth, credentials)
	case "apiKey", "api_key":
		applyAPIKey(req, auth, credentials)
	case "mutualTLS", "mutual_tls", "mtls":
		// mTLS is applied by providerClientForAuth because certificates belong
		// to the TLS transport, not request headers/query/cookies.
		return
	}
}

func applyHTTPAuth(req *http.Request, auth models.AuthConfig, credentials map[string]any) {
	scheme := strings.ToLower(auth.Scheme)
	if scheme == "bearer" {
		credValue, _ := credentials[auth.Name].(string)
		if credValue != "" {
			req.Header.Set("Authorization", "Bearer "+credValue)
		}
	} else if scheme == "basic" {
		user, _ := credentials[auth.Name+"_username"].(string)
		pass, _ := credentials[auth.Name+"_password"].(string)
		// Basic auth is only valid as a pair in our config/apply model. Refuse
		// partial manual bucket state instead of sending "user:" or ":pass".
		if user != "" && pass != "" {
			req.SetBasicAuth(user, pass)
		}
	} else {
		credValue, _ := credentials[auth.Name].(string)
		if credValue != "" {
			req.Header.Set("Authorization", auth.Scheme+" "+credValue)
		}
	}
}

func applyOAuth(req *http.Request, auth models.AuthConfig, credentials map[string]any) {
	credValue, _ := credentials[auth.Name].(string)
	if credValue != "" {
		req.Header.Set("Authorization", "Bearer "+credValue)
	}
}

func applyAPIKey(req *http.Request, auth models.AuthConfig, credentials map[string]any) {
	credValue, _ := credentials[auth.Name].(string)
	if credValue == "" {
		return
	}

	switch auth.Location {
	case "header":
		req.Header.Set(auth.KeyName, credValue)
	case "query":
		q := req.URL.Query()
		q.Set(auth.KeyName, credValue)
		req.URL.RawQuery = q.Encode()
	case "cookie":
		req.AddCookie(&http.Cookie{Name: auth.KeyName, Value: credValue})
	}
}
