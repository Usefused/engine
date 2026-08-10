package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
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
	client     *http.Client
	rateLimits store.ProviderRateLimitStore
}

// NewDispatcherWithProviderRateLimits wires the shared JetStream coordinator
// used by production. NewDispatcher remains available for executions without
// a provider quota policy.
func NewDispatcherWithProviderRateLimits(rateLimits store.ProviderRateLimitStore) *Dispatcher {
	dispatcher := NewDispatcher()
	dispatcher.rateLimits = rateLimits
	return dispatcher
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
	setRequestContentSpanAttributes(ctx, obj)
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
	span := trace.SpanFromContext(ctx)
	selectedAuths, err := selectRequestAuth(srv.AuthConfigs, obj.SecurityRequirements, credentials)
	if err != nil {
		recordAuthSelection(ctx, span, nil, "rejected")
		return 0, err
	}
	recordAuthSelection(ctx, span, selectedAuths, authSelectionOutcome(selectedAuths))
	req, err := prepareProviderRequest(ctx, srv, obj, params, credentials, bucketValues, selectedAuths, span)
	if err != nil {
		return 0, err
	}
	// Signal to the vendor that we want an SSE stream.
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	start := time.Now()
	defer func() { AddExecutionTiming(ctx, "provider_total", time.Since(start)) }()
	client, err := d.providerClientForAuth(selectedAuths, credentials)
	if err != nil {
		return 0, err
	}
	if _, err := d.awaitProviderRateLimit(ctx, srv, obj); err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	AddExecutionTiming(ctx, "provider_time_to_headers", time.Since(start))

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
	if err := d.syncProviderRateLimitResponse(ctx, srv, obj, resp); err != nil {
		return resp.StatusCode, err
	}

	observability.RequestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", srv.Name),
		attribute.String("endpoint", obj.Name),
		attribute.Int("status_code", resp.StatusCode),
	))

	if resp.StatusCode >= 400 {
		// Provider bodies can contain tenant data and do not belong in execution
		// errors, OTEL, or Activity receipts.
		return resp.StatusCode, fmt.Errorf("provider returned status %d", resp.StatusCode)
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
	return d.runPagination(ctx, srv, obj, params, credentials, bucketValues, stream)
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
		AddExecutionCount(ctx, "provider_attempt_count", 1)
		if resetter, ok := stream.(interface{ ResetForRetry() }); ok {
			resetter.ResetForRetry()
		}
		if attempt > 0 {
			observability.RetriesTotal.Add(ctx, 1)
			span.SetAttributes(attribute.Int("retry_count", attempt))
		}

		status, err := d.executeOnce(ctx, srv, obj, params, credentials, bucketValues, stream, attempt)

		// A response body or post-response accounting failure must not replay a
		// request the provider may already have committed.
		if !retryableProviderAttempt(status) {
			return status, err
		}

		lastErr = err
		lastStatus = status

		if attempt < maxRetries {
			if delay := retrypolicy.BackoffDuration(policy.Strategy, policy.BackoffMs, attempt); delay > 0 {
				if err := waitForRetry(ctx, delay); err != nil {
					return lastStatus, err
				}
			}
		}
	}

	return lastStatus, normalizeError(lastErr, lastStatus)
}

func retryableProviderAttempt(status int) bool {
	return status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
		attribute.String("provider.protocol", providerProtocol(obj)),
		attribute.String("graphql.operation.kind", obj.OperationKind),
	))
	defer span.End()
	selectedAuths, err := selectRequestAuth(srv.AuthConfigs, obj.SecurityRequirements, credentials)
	if err != nil {
		recordAuthSelection(ctx, span, nil, "rejected")
		return 0, err
	}
	recordAuthSelection(ctx, span, selectedAuths, authSelectionOutcome(selectedAuths))

	req, err := prepareProviderRequest(ctx, srv, obj, params, credentials, bucketValues, selectedAuths, span)
	if err != nil {
		return 0, err
	}

	providerStarted := time.Now()
	client, err := d.providerClientForAuth(selectedAuths, credentials)
	if err != nil {
		AddExecutionTiming(ctx, "provider_total", time.Since(providerStarted))
		return 0, err
	}
	if _, err := d.awaitProviderRateLimit(ctx, srv, obj); err != nil {
		AddExecutionTiming(ctx, "provider_total", time.Since(providerStarted))
		return 0, err
	}
	resp, err := client.Do(req)
	AddExecutionTiming(ctx, "provider_time_to_headers", time.Since(providerStarted))
	if err != nil {
		AddExecutionTiming(ctx, "provider_total", time.Since(providerStarted))
		return 0, fmt.Errorf("network request failed: %w", err)
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	if err := d.syncProviderRateLimitResponse(ctx, srv, obj, resp); err != nil {
		AddExecutionTiming(ctx, "provider_total", time.Since(providerStarted))
		return resp.StatusCode, err
	}
	if sink, ok := stream.(interface {
		CaptureResponseMetadata(http.Header, *neturl.URL)
	}); ok {
		sink.CaptureResponseMetadata(resp.Header, resp.Request.URL)
	}

	status, err := streamResponseBody(ctx, resp.Body, resp.StatusCode, stream)
	AddExecutionTiming(ctx, "provider_total", time.Since(providerStarted))
	// Aggregate timings belong to the logical execution span. This span's own
	// duration represents one provider attempt/page; attaching the accumulator
	// here would make later pagination spans look progressively slower.
	return status, err
}

func prepareProviderRequest(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	selectedAuths models.AuthConfigs,
	span trace.Span,
) (*http.Request, error) {
	started := time.Now()
	defer func() { AddExecutionTiming(ctx, "provider_request_prepare", time.Since(started)) }()
	reqURL, headers, bodyReader, err := prepareRequestParts(srv, obj, params, bucketValues)
	if err != nil {
		span.SetStatus(codes.Error, "request_prepare_failed")
		return nil, fmt.Errorf("failed to prepare request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, obj.Method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req = requestWithProviderHTTPTrace(ctx, req, srv, obj, span)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	for key, value := range srv.DefaultHeaders {
		if req.Header.Get(key) == "" {
			req.Header.Set(key, value)
		}
	}
	applySelectedAuth(req, selectedAuths, credentials)
	return req, nil
}

func streamResponseBody(ctx context.Context, body io.Reader, status int, stream ResponseStream) (int, error) {
	bodyStarted := time.Now()
	var sendDuration time.Duration
	defer func() {
		bodyDuration := time.Since(bodyStarted)
		AddExecutionTiming(ctx, "provider_body_stream", bodyDuration)
		AddExecutionTiming(ctx, "engine_stream_send", sendDuration)
		if bodyDuration > sendDuration {
			AddExecutionTiming(ctx, "provider_body_read", bodyDuration-sendDuration)
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
	applyForcedPathBindings(&reqURL, obj.Parameters, bucketValues)
	extractCustomHeaders(params, headerParams)
	reqURL = mapParameters(reqURL, obj, params, queryParams, headerParams, bodyParams)
	applyDefaultBindings(&reqURL, queryParams, headerParams, bodyParams, obj.Parameters, bucketValues)
	applyForcedBindings(&reqURL, queryParams, headerParams, bodyParams, obj.Parameters, bucketValues)

	reqURL = appendQueryParams(reqURL, queryParams)

	bodyReader, err := buildRequestBody(obj, headerParams, bodyParams)
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
func applyDefaultBindings(reqURL *string, query neturl.Values, headers map[string]string, body map[string]any, parameters models.Parameters, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if normalizedBindingMode(binding.Mode) != "default" || binding.Location == "base_url" {
			continue
		}
		applyBinding(reqURL, query, headers, body, parameters, binding, false)
	}
}

// applyForcedBindings runs after caller mapping because tenant-routing values
// must not be overridden by SDK input.
func applyForcedBindings(reqURL *string, query neturl.Values, headers map[string]string, body map[string]any, parameters models.Parameters, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if normalizedBindingMode(binding.Mode) != "force" || binding.Location == "base_url" || binding.Location == "path" {
			continue
		}
		applyBinding(reqURL, query, headers, body, parameters, binding, true)
	}
}

// applyForcedPathBindings consumes placeholders before caller mapping so
// selected resource context wins without a second string-replacement pass.
func applyForcedPathBindings(reqURL *string, parameters models.Parameters, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if normalizedBindingMode(binding.Mode) == "force" && binding.Location == "path" {
			applyPathBinding(reqURL, binding.KeyName, binding.Value, pathEncodingForParameter(parameters, binding.KeyName), true)
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
func applyBinding(reqURL *string, query neturl.Values, headers map[string]string, body map[string]any, parameters models.Parameters, binding store.BucketValue, force bool) {
	switch strings.ToLower(binding.Location) {
	case "header":
		applyHeaderBinding(headers, binding.KeyName, binding.Value, force)
	case "query":
		applyQueryBinding(query, binding.KeyName, binding.Value, force)
	case "path":
		applyPathBinding(reqURL, binding.KeyName, binding.Value, pathEncodingForParameter(parameters, binding.KeyName), force)
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

// applyPathBinding substitutes only the named placeholder and leaves unmatched
// optional defaults alone, avoiding accidental whole-URL string replacement.
func applyPathBinding(reqURL *string, name, value, pathEncoding string, force bool) {
	placeholder := regexp.MustCompile(`(?i)\{` + regexp.QuoteMeta(name) + `\}`)
	if !force && !placeholder.MatchString(*reqURL) {
		return
	}
	if placeholder.MatchString(*reqURL) {
		*reqURL = placeholder.ReplaceAllString(*reqURL, encodePathParameter(value, pathEncoding))
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
		reqURL = applyParam(in, k, v, valStr, reqURL, obj.Method, pathEncodingForParameter(obj.Parameters, k), queryParams, headerParams, bodyParams)
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

func applyParam(in, key string, val any, valStr, reqURL, method, pathEncoding string, queryParams neturl.Values, headerParams map[string]string, bodyParams map[string]any) string {
	// Route the parameter to the appropriate location
	switch in {
	case "path":
		// Replace the path variable placeholder with the URL-encoded value
		encodedVal := encodePathParameter(valStr, pathEncoding)
		return regexp.MustCompile(fmt.Sprintf(`(?i)\{%s\}`, regexp.QuoteMeta(key))).ReplaceAllString(reqURL, encodedVal)
	case "query":
		// SDK parameters replace defaults at their declared scalar target.
		queryParams.Set(key, valStr)
	case "header":
		// Add the parameter to the request headers
		headerParams[key] = valStr
	case "body":
		bodyParams[key] = val
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

func pathEncodingForParameter(parameters models.Parameters, name string) string {
	for _, parameter := range parameters {
		if strings.EqualFold(parameter.Name, name) {
			return parameter.PathEncoding
		}
	}
	return ""
}

func encodePathParameter(value, pathEncoding string) string {
	// Whole-value escaping remains the safe default. Slash preservation must be
	// reviewed explicitly, and each segment is still escaped independently so
	// reserved characters cannot become a query, fragment, or adjacent segment.
	if pathEncoding != models.PathEncodingPreserveSlashes {
		return neturl.PathEscape(value)
	}
	segments := strings.Split(value, "/")
	for index, segment := range segments {
		segments[index] = neturl.PathEscape(segment)
	}
	return strings.Join(segments, "/")
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

func buildRequestBody(obj *models.IntegrationObject, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	// GraphQL is handled before GET/DELETE suppression because its query document
	// is part of the required request body, regardless of the declared method.
	if providerProtocol(obj) == models.ProviderProtocolGraphQL {
		if obj.GraphQLQuery == nil || strings.TrimSpace(*obj.GraphQLQuery) == "" {
			return nil, errors.New("GraphQL operation is missing its query document")
		}
		return buildGraphQLRequestBody(obj, headerParams, bodyParams)
	}
	return buildRESTRequestBody(obj.RequestContent, headerParams, bodyParams)
}

func buildRESTRequestBody(content *models.RequestContent, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	// The reviewed contract is the only authority for whether a REST body
	// exists. Method and service defaults cannot recreate a body the importer
	// deliberately left absent.
	if content == nil {
		return nil, nil
	}
	if strings.TrimSpace(content.MediaType) == "" {
		return nil, errors.New("request content media_type is required")
	}
	if err := validateRequiredRequestBody(content, bodyParams); err != nil {
		return nil, err
	}
	setAuthoritativeContentType(headerParams, content.MediaType)
	switch content.Serialization {
	case models.RequestSerializationJSON:
		return buildJSONRequestBody(bodyParams)
	case models.RequestSerializationForm:
		return buildFormRequestBody(bodyParams), nil
	case models.RequestSerializationMultipart:
		return buildMultipartRequestBody(content, headerParams, bodyParams)
	case models.RequestSerializationRaw:
		return buildRawRequestBody(content, bodyParams)
	default:
		return nil, fmt.Errorf("unknown request serialization %q", content.Serialization)
	}
}

func validateRequiredRequestBody(content *models.RequestContent, bodyParams map[string]any) error {
	if content.Serialization != models.RequestSerializationRaw && content.Required && len(bodyParams) == 0 {
		return errors.New("missing required request body")
	}
	return nil
}

func setAuthoritativeContentType(headers map[string]string, mediaType string) {
	for key := range headers {
		if strings.EqualFold(key, "Content-Type") {
			delete(headers, key)
		}
	}
	headers["Content-Type"] = mediaType
}

func buildJSONRequestBody(bodyParams map[string]any) (io.Reader, error) {
	bodyBytes, err := json.Marshal(bodyParams)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON body: %w", err)
	}
	return bytes.NewReader(bodyBytes), nil
}

func buildFormRequestBody(bodyParams map[string]any) io.Reader {
	formValues := neturl.Values{}
	for key, value := range bodyParams {
		formValues.Add(key, fmt.Sprintf("%v", value))
	}
	return strings.NewReader(formValues.Encode())
}

func buildMultipartRequestBody(content *models.RequestContent, headers map[string]string, bodyParams map[string]any) (io.Reader, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	contentType, err := multipartRequestContentType(content.MediaType, writer.Boundary())
	if err != nil {
		return nil, err
	}
	for name, value := range bodyParams {
		payload, partContentType, err := encodeMultipartPart(name, value, content.Parts[name])
		if err != nil {
			return nil, err
		}
		part, err := writer.CreatePart(multipartPartHeader(name, partContentType))
		if err != nil {
			return nil, fmt.Errorf("failed to create multipart part %q", name)
		}
		if _, err := part.Write(payload); err != nil {
			return nil, fmt.Errorf("failed to write multipart part %q", name)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("failed to finalize multipart request")
	}
	setAuthoritativeContentType(headers, contentType)
	return bytes.NewReader(body.Bytes()), nil
}

func encodeMultipartPart(name string, value any, part models.RequestPart) ([]byte, string, error) {
	if part.BinaryEncoding != "" {
		return encodeMultipartBinaryPart(name, value, part)
	}
	contentType, err := validatedPartContentType(name, part.ContentType)
	if err != nil {
		return nil, "", err
	}
	switch typed := value.(type) {
	case string:
		return []byte(typed), contentType, nil
	case []byte:
		return typed, defaultContentType(contentType, "application/octet-stream"), nil
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return []byte(fmt.Sprintf("%v", typed)), contentType, nil
	default:
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, "", fmt.Errorf("multipart part %q is not JSON serializable", name)
		}
		return payload, defaultContentType(contentType, "application/json"), nil
	}
}

func encodeMultipartBinaryPart(name string, value any, part models.RequestPart) ([]byte, string, error) {
	if part.BinaryEncoding != models.RequestBinaryEncodingBase64 {
		return nil, "", fmt.Errorf("multipart part %q has unsupported binary_encoding %q", name, part.BinaryEncoding)
	}
	encoded, ok := value.(string)
	if !ok {
		return nil, "", fmt.Errorf("multipart part %q requires a base64 string", name)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, "", fmt.Errorf("multipart part %q contains invalid base64", name)
	}
	contentType, err := validatedPartContentType(name, part.ContentType)
	if err != nil {
		return nil, "", err
	}
	return payload, defaultContentType(contentType, "application/octet-stream"), nil
}

func validatedPartContentType(name, contentType string) (string, error) {
	if contentType == "" {
		return "", nil
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return "", fmt.Errorf("multipart part %q has invalid content_type", name)
	}
	return contentType, nil
}

func defaultContentType(contentType, fallback string) string {
	if contentType != "" {
		return contentType
	}
	return fallback
}

func multipartPartHeader(name, contentType string) textproto.MIMEHeader {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name}))
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return header
}

func multipartRequestContentType(importedMediaType, boundary string) (string, error) {
	mediaType, parameters, err := mime.ParseMediaType(importedMediaType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return "", errors.New("multipart request media_type must be multipart/form-data")
	}
	parameters["boundary"] = boundary
	return mime.FormatMediaType(mediaType, parameters), nil
}

func buildRawRequestBody(content *models.RequestContent, bodyParams map[string]any) (io.Reader, error) {
	name := strings.TrimSpace(content.PayloadParameter)
	if name == "" {
		return nil, errors.New("raw request payload_parameter is required")
	}
	value, ok := bodyParams[name]
	if !ok {
		return nil, fmt.Errorf("missing raw request payload parameter %q", name)
	}
	if len(bodyParams) != 1 {
		return nil, fmt.Errorf("raw request contains parameters outside payload_parameter %q", name)
	}
	payload, err := encodeRawPayload(name, value, content.BinaryEncoding)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(payload), nil
}

func encodeRawPayload(name string, value any, binaryEncoding string) ([]byte, error) {
	if binaryEncoding == models.RequestBinaryEncodingBase64 {
		return decodeRawBase64(name, value)
	}
	if binaryEncoding != "" {
		return nil, fmt.Errorf("raw request payload %q has unsupported binary_encoding %q", name, binaryEncoding)
	}
	switch typed := value.(type) {
	case string:
		return []byte(typed), nil
	case []byte:
		return typed, nil
	default:
		return nil, fmt.Errorf("raw request payload %q must be a string or byte array", name)
	}
}

func decodeRawBase64(name string, value any) ([]byte, error) {
	encoded, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("raw request payload %q requires a base64 string", name)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("raw request payload %q contains invalid base64", name)
	}
	return payload, nil
}

func setRequestContentSpanAttributes(ctx context.Context, obj *models.IntegrationObject) {
	serialization, mediaFamily := requestContentTelemetry(obj)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("request.serialization", serialization),
		attribute.String("request.media_family", mediaFamily),
	)
}

func requestContentTelemetry(obj *models.IntegrationObject) (string, string) {
	if providerProtocol(obj) == models.ProviderProtocolGraphQL {
		return "graphql", "json"
	}
	if obj.RequestContent == nil {
		return "none", "none"
	}
	return boundedRequestSerialization(obj.RequestContent.Serialization), requestMediaFamily(obj.RequestContent.MediaType)
}

func boundedRequestSerialization(serialization string) string {
	switch serialization {
	case models.RequestSerializationJSON,
		models.RequestSerializationForm,
		models.RequestSerializationMultipart,
		models.RequestSerializationRaw:
		return serialization
	default:
		return "unknown"
	}
}

func requestMediaFamily(value string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	switch {
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return "json"
	case mediaType == "application/x-www-form-urlencoded":
		return "form"
	case strings.HasPrefix(mediaType, "multipart/"):
		return "multipart"
	case strings.HasPrefix(mediaType, "text/"):
		return "text"
	case mediaType == "application/octet-stream":
		return "binary"
	default:
		return "other"
	}
}

// providerProtocol accepts legacy in-memory operations that predate the field
// so direct callers and rolling Engine upgrades do not silently lose GraphQL
// request construction while refreshed snapshots become explicit.
func providerProtocol(obj *models.IntegrationObject) string {
	if obj.ProviderProtocol != "" {
		return obj.ProviderProtocol
	}
	if obj.GraphQLQuery != nil {
		return models.ProviderProtocolGraphQL
	}
	return models.ProviderProtocolREST
}

// Keep direct dispatch aligned with generated SDKs by using the same standard
// envelope and omitting variables when the operation has none.
func buildGraphQLRequestBody(obj *models.IntegrationObject, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	envelope := map[string]any{"query": *obj.GraphQLQuery}
	variables := graphQLVariables(bodyParams)
	if len(variables) > 0 {
		envelope["variables"] = variables
	}
	bodyBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal GraphQL request body: %w", err)
	}
	setAuthoritativeContentType(headerParams, "application/json")
	return bytes.NewReader(bodyBytes), nil
}

// Generated SDKs flatten their GraphQL envelope into Engine params. Unwrap
// only that recognizable shape and ignore its query document: the reviewed
// Registry operation remains the authority, so an SDK token cannot substitute
// a different provider operation while reusing an allowed endpoint name.
func graphQLVariables(bodyParams map[string]any) map[string]any {
	if !isGeneratedGraphQLEnvelope(bodyParams) {
		return bodyParams
	}
	variables, _ := bodyParams["variables"].(map[string]any)
	return variables
}

func isGeneratedGraphQLEnvelope(bodyParams map[string]any) bool {
	if len(bodyParams) == 0 || len(bodyParams) > 3 {
		return false
	}
	if _, ok := bodyParams["query"].(string); !ok {
		return false
	}
	if len(bodyParams) == 1 {
		return true
	}
	variables, ok := bodyParams["variables"]
	if !ok {
		return false
	}
	if variables != nil {
		if _, ok := variables.(map[string]any); !ok {
			return false
		}
	}
	_, hasOperationName := bodyParams["operationName"]
	return len(bodyParams) == 2 || hasOperationName
}

func applyHTTPAuth(req *http.Request, auth models.AuthConfig, credentials map[string]any) {
	scheme := strings.ToLower(auth.Scheme)
	if scheme == "bearer" {
		credValue, _ := credentials[auth.Name].(string)
		if credValue != "" {
			req.Header.Set("Authorization", "Bearer "+credValue)
		}
	} else if scheme == "basic" {
		applyBasicAuth(req, auth, credentials)
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
