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
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
				slog.String("method_family", boundedProviderMethod(obj.Method)),
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
	intent, hasIntent := PaginationIntentFromContext(ctx)
	// Request pagination controls fail before content selection or provider I/O and may only tighten reviewed pagination.
	if hasIntent {
		if err := ValidatePaginationIntentPolicy(&intent, obj.Pagination); err != nil {
			return 0, err
		}
	}
	selectedContent, selectionOutcome, err := SelectRequestContent(obj.RequestContent)
	setRequestContentSpanAttributes(ctx, obj, selectedContent, selectionOutcome)
	if err != nil {
		return 0, err
	}
	if selectedContent != nil && selectedContent.UploadWorkflow != nil {
		return d.executeUploadWorkflow(ctx, srv, obj, params, credentials, bucketValues, stream)
	}
	if obj.Method == "SOAP" {
		return d.executeSOAP(ctx, srv, obj, params, credentials, bucketValues, stream)
	}
	if obj.Pagination != nil {
		return d.executePaginated(ctx, srv, obj, params, credentials, bucketValues, stream)
	}
	return d.executeWithRetries(ctx, srv, obj, params, credentials, bucketValues, stream)
}

func sseContract(representation *models.ResponseRepresentation) *models.SSEResponseContract {
	if representation == nil {
		return nil
	}
	return representation.SSE
}

// parseSSEEvents reads an SSE stream line-by-line and sends each complete event
// to the stream. Lines starting with "data:" are accumulated into an event; a
// blank line signals the end of one event. Empty data remains an empty string,
// and only a contract-declared sentinel terminates parsing. All other SSE
// fields are ignored because item_schema describes only the data payload.
func parseSSEEvents(body io.Reader, stream ResponseStream, contracts ...*models.SSEResponseContract) error {
	scanner := bufio.NewScanner(body)
	decoder := newSSEEventDecoder(stream, contracts)
	for scanner.Scan() {
		done, err := decoder.consume(scanner.Text())
		if err != nil || done {
			return err
		}
	}
	if _, err := decoder.flush(); err != nil {
		return err
	}
	return scanner.Err()
}

type sseEventDecoder struct {
	stream   ResponseStream
	sentinel *string
	data     strings.Builder
	hasData  bool
}

func newSSEEventDecoder(stream ResponseStream, contracts []*models.SSEResponseContract) *sseEventDecoder {
	decoder := &sseEventDecoder{stream: stream}
	if len(contracts) > 0 && contracts[0] != nil {
		decoder.sentinel = contracts[0].DoneSentinel
	}
	return decoder
}

func (decoder *sseEventDecoder) consume(line string) (bool, error) {
	if line == "" {
		return decoder.flush()
	}
	if !strings.HasPrefix(line, "data:") {
		return false, nil
	}
	decoder.appendData(strings.TrimPrefix(line, "data:"))
	return false, nil
}

func (decoder *sseEventDecoder) appendData(payload string) {
	if strings.HasPrefix(payload, " ") {
		payload = payload[1:]
	}
	if decoder.hasData {
		decoder.data.WriteByte('\n')
	}
	decoder.hasData = true
	decoder.data.WriteString(payload)
}

func (decoder *sseEventDecoder) flush() (bool, error) {
	if !decoder.hasData {
		return false, nil
	}
	payload := decoder.data.String()
	decoder.data.Reset()
	decoder.hasData = false
	if decoder.sentinel != nil && payload == *decoder.sentinel {
		return true, nil
	}
	if err := decoder.stream.Send(sseItemChunk(payload)); err != nil {
		return false, fmt.Errorf("failed to stream SSE event: %w", err)
	}
	return false, nil
}

func sseItemChunk(payload string) []byte {
	if payload == "" {
		// Protobuf omits zero-length bytes, so JSON's empty string is the one
		// lossless frame that generated clients can distinguish from metadata.
		return []byte(`""`)
	}
	return []byte(payload)
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
	ctx = withMultipartReplayBoundary(ctx)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.retry")
	defer span.End()
	if srv.RetryConfig == nil {
		AddExecutionCount(ctx, "provider_attempt_count", 1)
		return d.executeOnce(ctx, srv, obj, params, credentials, bucketValues, stream, 0)
	}
	return d.executeWithRetriesV3(ctx, span, srv, obj, params, credentials, bucketValues, stream)
}

func commitAttemptResult(stream *deferredResponseStream, status int, err error) (int, error) {
	if commitErr := stream.Commit(); commitErr != nil {
		return status, commitErr
	}
	return status, err
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
		attribute.String("http.method_family", boundedProviderMethod(obj.Method)),
		attribute.String("peer.service", srv.Name),
		attribute.String("provider.protocol", providerProtocol(obj)),
		attribute.String("graphql.operation.kind", obj.OperationKind),
	))
	defer span.End()
	req, selectedAuths, err := prepareSecuredProviderRequest(ctx, span, srv, obj, params, credentials, bucketValues)
	if err != nil {
		return 0, err
	}
	defer closeProviderRequestBody(req)
	setProviderAcceptHeader(req, obj.Responses)

	providerStarted := time.Now()
	defer func() { AddExecutionTiming(ctx, "provider_total", time.Since(providerStarted)) }()
	client, err := d.providerClientForAuth(selectedAuths, credentials)
	if err != nil {
		return 0, err
	}
	_, permit, err := d.awaitProviderRateLimitPermit(ctx, srv, obj)
	if err != nil {
		return 0, err
	}
	defer func() { d.releaseProviderRateLimit(ctx, permit) }()
	resp, err := client.Do(req)
	if err != nil {
		recordRetryTransportError(ctx, err)
		return 0, &providerTransportError{err: err}
	}
	resp, err = retryHTTPChallengeWithDo(ctx, req, resp, selectedAuths, credentials, func(retry *http.Request) (*http.Response, error) {
		d.releaseProviderRateLimit(ctx, permit)
		permit = ratelimitpolicy.ReleaseRequest{}
		_, nextPermit, acquireErr := d.awaitProviderRateLimitPermit(ctx, srv, obj)
		if acquireErr != nil {
			return nil, acquireErr
		}
		permit = nextPermit
		AddExecutionCount(ctx, "provider_attempt_count", 1)
		response, doErr := client.Do(retry)
		if doErr != nil {
			return nil, &providerTransportError{err: doErr}
		}
		return response, nil
	})
	AddExecutionTiming(ctx, "provider_time_to_headers", time.Since(providerStarted))
	if err != nil {
		recordRetryChallengeTransportError(ctx, err)
		return 0, fmt.Errorf("HTTP authentication challenge failed: %w", err)
	}
	return d.completeProviderAttempt(ctx, span, srv, obj, resp, stream, true)
}

// completeProviderAttempt owns response accounting and decoding after the
// provider request is final. Keeping that boundary explicit prevents retry
// orchestration from publishing headers or body chunks from discarded attempts.
func (d *Dispatcher) completeProviderAttempt(ctx context.Context, span trace.Span, srv *models.Service, obj *models.IntegrationObject, resp *http.Response, stream ResponseStream, quotaPending bool) (int, error) {
	defer resp.Body.Close()
	recordRetryResponse(ctx, resp.Header)
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	family, selectionOutcome := recordResponseMedia(span, obj, resp.StatusCode, resp.Header.Get("Content-Type"))
	selectedFamily := selectedResponseMediaFamily(family, selectionOutcome)
	recordRetryResponseMedia(ctx, selectedFamily)
	if err := SendResponseContract(stream, resp.StatusCode, selectedFamily); err != nil {
		return resp.StatusCode, err
	}
	if quotaPending {
		bodyValues, err := captureQuotaSignalsForSelectedResponse(srv.RateLimit, resp, selectedFamily)
		if err != nil {
			return resp.StatusCode, err
		}
		if err := d.syncProviderRateLimitResponse(ctx, srv, obj, resp, bodyValues); err != nil {
			return resp.StatusCode, err
		}
	}
	captureResponseStreamMetadata(stream, resp)
	// Aggregate timings belong to the logical execution span. This span's own
	// duration represents one provider attempt/page; attaching the accumulator
	// here would make later pagination spans look progressively slower.
	return streamSelectedResponseBody(ctx, resp, obj.Responses, selectedFamily, stream)
}

func captureQuotaSignalsForSelectedResponse(config *ratelimitpolicy.Config, response *http.Response, mediaFamily string) (map[string]any, error) {
	if mediaFamily == "sse" {
		// Reading a live stream to discover body counters would delay or exhaust
		// it. Header signals remain available to the shared sync path.
		return nil, nil
	}
	return captureQuotaSignalBody(config, response)
}

func selectedResponseMediaFamily(actualFamily, selectionOutcome string) string {
	if selectionOutcome != "matched" {
		// An unreviewed wire type must remain opaque. Publishing the provider's
		// apparent family would make generated clients execute decoding semantics
		// that the imported contract never authorized.
		return "unknown"
	}
	return actualFamily
}

func streamSelectedResponseBody(ctx context.Context, response *http.Response, responses models.Responses, family string, stream ResponseStream) (int, error) {
	if family != "sse" || response.StatusCode >= 400 {
		return streamResponseBody(ctx, response.Body, response.StatusCode, stream)
	}
	representation, matched := responseRepresentationForStatusAndMedia(responses, response.StatusCode, response.Header.Get("Content-Type"))
	if !matched || representation.SSE == nil {
		return streamResponseBody(ctx, response.Body, response.StatusCode, stream)
	}
	return response.StatusCode, parseSSEEvents(response.Body, stream, sseContract(representation))
}

func captureResponseStreamMetadata(stream ResponseStream, response *http.Response) {
	if sink, ok := stream.(interface {
		CaptureResponseMetadata(http.Header, *neturl.URL)
	}); ok {
		sink.CaptureResponseMetadata(response.Header, response.Request.URL)
	}
}

func closeProviderRequestBody(request *http.Request) {
	if request != nil && request.Body != nil {
		_ = request.Body.Close()
	}
}

func prepareSecuredProviderRequest(
	ctx context.Context,
	span trace.Span,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
) (*http.Request, models.AuthConfigs, error) {
	selectedAuths, err := selectRequestAuth(srv.AuthConfigs, obj.SecurityRequirements, credentials)
	if err != nil {
		recordAuthSelection(ctx, span, nil, "rejected")
		return nil, nil, err
	}
	recordAuthSelection(ctx, span, selectedAuths, authSelectionOutcome(selectedAuths))
	if err := applySelectedSecurityServer(srv, obj, selectedAuths, bucketValues); err != nil {
		return nil, nil, err
	}
	req, err := prepareProviderRequest(ctx, srv, obj, params, credentials, bucketValues, selectedAuths, span)
	if err != nil {
		return nil, nil, err
	}
	recordRetryRequest(ctx, req)
	return req, selectedAuths, nil
}

func boundedProviderMethod(method string) string {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, "QUERY", "SOAP":
		return strings.ToLower(method)
	default:
		return "custom"
	}
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
	span.SetAttributes(attribute.String("http.server.source", boundedServerSource(srv.ServerSource)))
	reqURL, headers, bodyReader, err := prepareRequestPartsWithContext(ctx, srv, obj, params, bucketValues)
	if err != nil {
		span.SetAttributes(attribute.String("http.parameter_serialization.outcome", "rejected"))
		span.SetStatus(codes.Error, "request_prepare_failed")
		return nil, fmt.Errorf("failed to prepare request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, obj.Method, reqURL, bodyReader)
	if err != nil {
		closeRequestReader(bodyReader)
		span.SetAttributes(attribute.String("http.parameter_serialization.outcome", "rejected"))
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
	if err := rejectAuthCookieCollision(req, selectedAuths); err != nil {
		closeProviderRequestBody(req)
		span.SetAttributes(attribute.String("http.parameter_serialization.outcome", "rejected"))
		return nil, err
	}
	if err := applySelectedAuthChecked(req, selectedAuths, credentials); err != nil {
		closeProviderRequestBody(req)
		return nil, err
	}
	span.SetAttributes(attribute.String("http.parameter_serialization.outcome", "success"))
	return req, nil
}

func closeRequestReader(reader io.Reader) {
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
}

func boundedServerSource(source string) string {
	switch source {
	case "operation", "service", "connection_resource":
		return source
	default:
		return "service"
	}
}

func rejectAuthCookieCollision(req *http.Request, auths models.AuthConfigs) error {
	for _, auth := range auths {
		if auth.Location != "cookie" {
			continue
		}
		if _, err := req.Cookie(auth.KeyName); err == nil {
			// Caller cookies and authentication remain separate trust domains; a
			// collision is rejected so neither can silently shadow the other.
			return errors.New("request cookie conflicts with API-key authentication")
		}
	}
	return nil
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
	return prepareRequestPartsWithContext(context.Background(), srv, obj, params, bucketValues)
}

// prepareRequestPartsWithContext resolves forced path bindings before mapping
// caller input because later whole-query parsing would otherwise escape braces.
func prepareRequestPartsWithContext(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, params map[string]any, bucketValues []store.BucketValue) (string, map[string]string, io.Reader, error) {
	if err := validateResolvedBindingHeaders(bucketValues); err != nil {
		return "", nil, nil, err
	}
	selectedContent, _, err := SelectRequestContent(obj.RequestContent)
	if err != nil {
		return "", nil, nil, err
	}
	if err := ValidateDeclaredExecutionParameters(obj.Parameters, selectedContent, params); err != nil {
		return "", nil, nil, err
	}
	queryParams := queryParameters{}
	headerParams := make(map[string]string)
	bodyParams := make(map[string]any)
	cookieParams := make(map[string]string)

	baseURL := bindingBaseURL(srv.BaseURL, bucketValues)
	reqURL := buildBaseURL(obj.Path, baseURL)
	applyForcedPathBindings(&reqURL, obj.Parameters, bucketValues)
	extractCustomHeaders(params, headerParams)
	reqURL, err = mapParameters(reqURL, obj, params, queryParams, headerParams, cookieParams, bodyParams)
	if err != nil {
		return "", nil, nil, err
	}
	applyDefaultBindings(&reqURL, queryParams, headerParams, bodyParams, obj.Parameters, bucketValues)
	applyForcedBindings(&reqURL, queryParams, headerParams, bodyParams, obj.Parameters, bucketValues)
	if err := rejectUnresolvedPathPlaceholder(reqURL); err != nil {
		return "", nil, nil, err
	}
	if hasWholeQueryStringParameter(obj.Parameters) && len(queryParams) > 0 {
		return "", nil, nil, errors.New("querystring parameter cannot be combined with query bindings")
	}

	reqURL = appendQueryParams(reqURL, queryParams)
	if value := cookieHeader(cookieParams); value != "" {
		headerParams["Cookie"] = value
	}

	bodyReader, err := buildSelectedRequestBodyWithContext(ctx, obj, selectedContent, headerParams, bodyParams)
	if err != nil {
		return "", nil, nil, err
	}

	return reqURL, headerParams, bodyReader, nil
}

func rejectUnresolvedPathPlaceholder(requestURL string) error {
	match := regexp.MustCompile(`\{([^{}]+)\}`).FindStringSubmatch(requestURL)
	if len(match) > 1 {
		return fmt.Errorf("undeclared or unresolved path parameter %q", match[1])
	}
	return nil
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
func applyDefaultBindings(reqURL *string, query queryParameters, headers map[string]string, body map[string]any, parameters models.Parameters, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if binding.Mode != "default" || binding.Location == "base_url" {
			continue
		}
		applyBinding(reqURL, query, headers, body, parameters, binding, false)
	}
}

// applyForcedBindings runs after caller mapping because tenant-routing values
// must not be overridden by SDK input.
func applyForcedBindings(reqURL *string, query queryParameters, headers map[string]string, body map[string]any, parameters models.Parameters, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if binding.Mode != "force" || binding.Location == "base_url" || binding.Location == "path" {
			continue
		}
		applyBinding(reqURL, query, headers, body, parameters, binding, true)
	}
}

// applyForcedPathBindings consumes placeholders before caller mapping so
// selected resource context wins without a second string-replacement pass.
func applyForcedPathBindings(reqURL *string, parameters models.Parameters, bindings []store.BucketValue) {
	for _, binding := range bindings {
		if binding.Mode == "force" && binding.Location == "path" {
			pathEncoding, allowReserved := pathOptionsForParameter(parameters, binding.KeyName)
			applyPathBinding(reqURL, binding.KeyName, binding.Value, pathEncoding, allowReserved, true)
		}
	}
}

// applyBinding dispatches a validated binding to its transport owner instead of
// letting target-specific rules leak into resolution or storage.
func applyBinding(reqURL *string, query queryParameters, headers map[string]string, body map[string]any, parameters models.Parameters, binding store.BucketValue, force bool) {
	switch strings.ToLower(binding.Location) {
	case "header":
		applyHeaderBinding(headers, binding.KeyName, binding.Value, force)
	case "query":
		applyQueryBinding(query, binding.KeyName, binding.Value, force)
	case "path":
		pathEncoding, allowReserved := pathOptionsForParameter(parameters, binding.KeyName)
		applyPathBinding(reqURL, binding.KeyName, binding.Value, pathEncoding, allowReserved, force)
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
func applyQueryBinding(query queryParameters, name, value string, force bool) {
	if query.Has(name) && !force {
		return
	}
	// Set gives forced tenant context replacement semantics and prevents the
	// provider from receiving two conflicting account selectors.
	query.Set(name, value)
}

// applyPathBinding substitutes only the named placeholder and leaves unmatched
// optional defaults alone, avoiding accidental whole-URL string replacement.
func applyPathBinding(reqURL *string, name, value, pathEncoding string, allowReserved, force bool) {
	placeholder := regexp.MustCompile(`(?i)\{` + regexp.QuoteMeta(name) + `\}`)
	if !force && !placeholder.MatchString(*reqURL) {
		return
	}
	if placeholder.MatchString(*reqURL) {
		*reqURL = placeholder.ReplaceAllString(*reqURL, encodePathParameterValue(value, pathEncoding, allowReserved))
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

// mapParameters uses an explicit order because Go map iteration must never alter
// path substitution, whole-query ownership, or the final provider URL.
func mapParameters(reqURL string, obj *models.IntegrationObject, params map[string]any, queryParams queryParameters, headerParams, cookieParams map[string]string, bodyParams map[string]any) (string, error) {
	parameterDefinitions := make(map[string]models.Parameter)
	for _, p := range obj.Parameters {
		parameterDefinitions[p.Name] = p
	}

	for _, input := range orderedParameterInputs(params, parameterDefinitions) {
		var err error
		reqURL, err = applyParam(input.parameter, input.key, input.value, reqURL, queryParams, headerParams, cookieParams, bodyParams)
		if err != nil {
			return "", err
		}
	}
	return reqURL, nil
}

type parameterInput struct {
	key       string
	value     any
	parameter models.Parameter
}

func orderedParameterInputs(params map[string]any, definitions map[string]models.Parameter) []parameterInput {
	inputs := make([]parameterInput, 0, len(params))
	for key, value := range params {
		inputs = append(inputs, parameterInput{key: key, value: value, parameter: determineParameter(key, definitions)})
	}
	// Whole-query serialization parses and re-emits the URL. Resolving path
	// placeholders first prevents that ownership step from escaping `{name}`;
	// lexical ties make every other side effect deterministic as well.
	sort.Slice(inputs, func(i, j int) bool {
		left, right := parameterInputPriority(inputs[i]), parameterInputPriority(inputs[j])
		if left == right {
			return inputs[i].key < inputs[j].key
		}
		return left < right
	})
	return inputs
}

func parameterInputPriority(input parameterInput) int {
	switch input.parameter.In {
	case "path":
		return 0
	case "querystring":
		return 2
	default:
		return 1
	}
}

func determineParameter(key string, definitions map[string]models.Parameter) models.Parameter {
	if parameter, ok := definitions[key]; ok {
		return parameter
	}
	return models.Parameter{Name: key, In: "body"}
}

func applyParam(parameter models.Parameter, key string, val any, reqURL string, queryParams queryParameters, headerParams, cookieParams map[string]string, bodyParams map[string]any) (string, error) {
	switch parameter.In {
	case "path":
		return applyPathParameter(parameter, key, val, reqURL)
	case "query":
		return reqURL, serializeQueryParameter(parameter, val, queryParams)
	case "querystring":
		serialized, err := serializeWholeQueryString(parameter, val)
		if err != nil {
			return "", err
		}
		return appendWholeQueryString(reqURL, serialized)
	case "header":
		return reqURL, applyHeaderParameter(parameter, key, val, headerParams)
	case "cookie":
		return reqURL, serializeCookieParameter(parameter, val, cookieParams)
	case "body":
		bodyParams[key] = val
	default:
		return "", fmt.Errorf("unsupported parameter location %q", parameter.In)
	}
	return reqURL, nil
}

func applyPathParameter(parameter models.Parameter, key string, value any, requestURL string) (string, error) {
	serialized, err := serializePathParameter(parameter, value)
	if err != nil {
		return "", err
	}
	placeholder := regexp.MustCompile(fmt.Sprintf(`(?i)\{%s\}`, regexp.QuoteMeta(key)))
	return placeholder.ReplaceAllString(requestURL, serialized), nil
}

func applyHeaderParameter(parameter models.Parameter, key string, value any, headers map[string]string) error {
	serialized, err := serializeHeaderParameter(parameter, value)
	if err != nil {
		return err
	}
	headers[key] = serialized
	return nil
}

func pathOptionsForParameter(parameters models.Parameters, name string) (string, bool) {
	for _, parameter := range parameters {
		if strings.EqualFold(parameter.Name, name) {
			return parameter.PathEncoding, boolValue(parameter.Serialization.AllowReserved)
		}
	}
	return "", false
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

var safePathReserved = map[string]string{
	"%3A": ":", "%40": "@", "%21": "!", "%24": "$", "%26": "&", "%27": "'",
	"%28": "(", "%29": ")", "%2A": "*", "%2B": "+", "%2C": ",", "%3B": ";", "%3D": "=",
}

func encodePathParameterValue(value, pathEncoding string, allowReserved bool) string {
	encoded := encodePathParameter(value, pathEncoding)
	if !allowReserved {
		return encoded
	}
	for escaped, literal := range safePathReserved {
		encoded = strings.ReplaceAll(encoded, escaped, literal)
	}
	return encoded
}

func appendQueryParams(reqURL string, queryParams queryParameters) string {
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
	return buildRequestBodyWithContext(context.Background(), obj, headerParams, bodyParams)
}

func buildRequestBodyWithContext(ctx context.Context, obj *models.IntegrationObject, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	selected, _, err := SelectRequestContent(obj.RequestContent)
	if err != nil {
		return nil, err
	}
	return buildSelectedRequestBodyWithContext(ctx, obj, selected, headerParams, bodyParams)
}

func buildSelectedRequestBodyWithContext(ctx context.Context, obj *models.IntegrationObject, content *SelectedRequestRepresentation, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	// GraphQL is handled before GET/DELETE suppression because its query document
	// is part of the required request body, regardless of the declared method.
	if providerProtocol(obj) == models.ProviderProtocolGraphQL {
		if obj.GraphQLQuery == nil || strings.TrimSpace(*obj.GraphQLQuery) == "" {
			return nil, errors.New("GraphQL operation is missing its query document")
		}
		return buildGraphQLRequestBody(obj, headerParams, bodyParams)
	}
	if content == nil {
		return nil, nil
	}
	return buildSelectedRESTRequestBodyWithContext(ctx, content, headerParams, bodyParams)
}

func buildRESTRequestBody(content *models.RequestContent, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	return buildRESTRequestBodyWithContext(context.Background(), content, headerParams, bodyParams)
}

func buildRESTRequestBodyWithContext(ctx context.Context, content *models.RequestContent, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	// The reviewed contract is the only authority for whether a REST body
	// exists. Method and service defaults cannot recreate a body the importer
	// deliberately left absent.
	if content == nil {
		return nil, nil
	}
	selected, _, err := SelectRequestContent(content)
	if err != nil {
		return nil, err
	}
	return buildSelectedRESTRequestBodyWithContext(ctx, selected, headerParams, bodyParams)
}

func buildSelectedRESTRequestBodyWithContext(ctx context.Context, content *SelectedRequestRepresentation, headerParams map[string]string, bodyParams map[string]any) (io.Reader, error) {
	if strings.TrimSpace(content.MediaType) == "" {
		return nil, errors.New("request content media_type is required")
	}
	if err := validateRequiredRequestBody(content, bodyParams); err != nil {
		return nil, err
	}
	setAuthoritativeContentType(headerParams, content.MediaType)
	switch content.Serialization {
	case models.RequestSerializationJSON:
		return buildJSONRequestBody(content, bodyParams)
	case models.RequestSerializationForm:
		return buildFormRequestBody(content, bodyParams)
	case models.RequestSerializationMultipart:
		return buildMultipartRequestBodyWithContext(ctx, content, headerParams, bodyParams)
	case models.RequestSerializationRaw:
		if content.ItemSchema != nil {
			return buildSequentialRequestBody(content, bodyParams)
		}
		return buildRawRequestBody(content, bodyParams)
	default:
		return nil, fmt.Errorf("unknown request serialization %q", content.Serialization)
	}
}

func validateRequiredRequestBody(content *SelectedRequestRepresentation, bodyParams map[string]any) error {
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

func buildJSONRequestBody(content *SelectedRequestRepresentation, bodyParams map[string]any) (io.Reader, error) {
	payload, err := requestPayload(content, bodyParams, false, "JSON request")
	if err != nil {
		return nil, err
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON body: %w", err)
	}
	return bytes.NewReader(bodyBytes), nil
}

func buildFormRequestBody(content *SelectedRequestRepresentation, bodyParams map[string]any) (io.Reader, error) {
	formValues := queryParameters{}
	for key, value := range bodyParams {
		encoding, configured := content.Encoding[key]
		if !configured {
			formValues.Set(key, fmt.Sprint(value))
			continue
		}
		if hasNestedRequestEncoding(encoding) {
			return nil, errors.New("nested form encoding is unsupported")
		}
		parameter := models.Parameter{Name: key, In: "query", Serialization: models.ParameterSerialization{
			Style: encoding.Style, Explode: encoding.Explode, AllowReserved: encoding.AllowReserved,
		}}
		if err := serializeQueryParameter(parameter, value, formValues); err != nil {
			return nil, fmt.Errorf("form property %q: %w", key, err)
		}
	}
	return strings.NewReader(formValues.EncodeForm()), nil
}

func buildMultipartRequestBody(content *SelectedRequestRepresentation, headers map[string]string, bodyParams map[string]any) (io.Reader, error) {
	return buildMultipartRequestBodyWithContext(context.Background(), content, headers, bodyParams)
}

func buildMultipartRequestBodyWithContext(ctx context.Context, content *SelectedRequestRepresentation, headers map[string]string, bodyParams map[string]any) (io.Reader, error) {
	if hasPositionalMultipart(content) {
		return buildPositionalMultipartRequestBodyWithContext(ctx, content, headers, bodyParams)
	}
	var body bytes.Buffer
	writer, err := newReplayMultipartWriter(ctx, &body)
	if err != nil {
		return nil, err
	}
	contentType, err := multipartRequestContentType(content.MediaType, writer.Boundary())
	if err != nil {
		return nil, err
	}
	if err := writeNamedMultipartParts(writer, content, bodyParams); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("failed to finalize multipart request")
	}
	setAuthoritativeContentType(headers, contentType)
	return bytes.NewReader(body.Bytes()), nil
}

func newReplayMultipartWriter(ctx context.Context, destination io.Writer) (*multipart.Writer, error) {
	writer := multipart.NewWriter(destination)
	boundary := multipartReplayBoundary(ctx)
	if boundary == "" {
		return writer, nil
	}
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, errors.New("failed to reuse multipart retry boundary")
	}
	return writer, nil
}

func writeNamedMultipartParts(writer *multipart.Writer, content *SelectedRequestRepresentation, bodyParams map[string]any) error {
	for name, value := range bodyParams {
		encoding := requestPartEncoding(content, name)
		if hasNestedRequestEncoding(encoding) {
			// Named nested multipart needs property-aware dispatch. Failing here
			// protects direct dispatcher callers that bypass contract validation.
			return errors.New("named nested multipart encoding is unsupported")
		}
		payload, partContentType, err := encodeMultipartPart(name, value, encoding)
		if err != nil {
			return err
		}
		partHeader, err := multipartPartHeader(name, partContentType, encoding.Headers)
		if err != nil {
			return err
		}
		part, err := writer.CreatePart(partHeader)
		if err != nil {
			return fmt.Errorf("failed to create multipart part %q", name)
		}
		if _, err := part.Write(payload); err != nil {
			return fmt.Errorf("failed to write multipart part %q", name)
		}
	}
	return nil
}

type multipartReplayBoundaryKey struct{}

func withMultipartReplayBoundary(ctx context.Context) context.Context {
	if multipartReplayBoundary(ctx) != "" {
		return ctx
	}
	// Why: one opaque random boundary per logical execution keeps replay bytes
	// identical without making the boundary a digest of potentially secret data.
	boundary := multipart.NewWriter(io.Discard).Boundary()
	return context.WithValue(ctx, multipartReplayBoundaryKey{}, boundary)
}

func multipartReplayBoundary(ctx context.Context) string {
	boundary, _ := ctx.Value(multipartReplayBoundaryKey{}).(string)
	return boundary
}

func requestPartEncoding(content *SelectedRequestRepresentation, name string) models.RequestEncoding {
	if encoding, ok := content.Encoding[name]; ok {
		return encoding
	}
	return models.RequestEncoding{}
}

func encodeMultipartPart(name string, value any, encoding models.RequestEncoding) ([]byte, string, error) {
	if encoding.BinaryEncoding != "" {
		return encodeMultipartBinaryPart(name, value, encoding)
	}
	contentType, err := validatedPartContentType(name, encoding.ContentType)
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
		if encoding.Style != "" || encoding.Explode != nil {
			serialized, err := serializeMultipartStructuredValue(encoding, value)
			return []byte(serialized), contentType, err
		}
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, "", fmt.Errorf("multipart part %q is not JSON serializable", name)
		}
		return payload, defaultContentType(contentType, "application/json"), nil
	}
}

func serializeMultipartStructuredValue(encoding models.RequestEncoding, value any) (string, error) {
	parts, err := splitParameterValue(value)
	if err != nil {
		return "", err
	}
	parameter := models.Parameter{In: "query", Serialization: models.ParameterSerialization{Style: encoding.Style, Explode: encoding.Explode}}
	return serializeSimpleValue(parts, effectiveParameterExplode(parameter)), nil
}

func encodeMultipartBinaryPart(name string, value any, encoding models.RequestEncoding) ([]byte, string, error) {
	if encoding.BinaryEncoding != models.RequestBinaryEncodingBase64 {
		return nil, "", fmt.Errorf("multipart part %q has unsupported binary_encoding %q", name, encoding.BinaryEncoding)
	}
	encoded, ok := value.(string)
	if !ok {
		return nil, "", fmt.Errorf("multipart part %q requires a base64 string", name)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, "", fmt.Errorf("multipart part %q contains invalid base64", name)
	}
	contentType, err := validatedPartContentType(name, encoding.ContentType)
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

func multipartPartHeader(name, contentType string, headers map[string]models.HeaderContract) (textproto.MIMEHeader, error) {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name}))
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	for headerName, contract := range headers {
		value, ok := staticEncodingHeaderValue(contract)
		if !ok {
			continue
		}
		if strings.EqualFold(headerName, "Content-Disposition") || strings.EqualFold(headerName, "Content-Type") {
			return nil, fmt.Errorf("multipart part %q encoding header conflicts with framing", name)
		}
		if err := connectionprofile.ValidateResolvedHeader(headerName, value); err != nil {
			return nil, fmt.Errorf("multipart part %q encoding header is invalid", name)
		}
		header.Set(headerName, value)
	}
	return header, nil
}

func staticEncodingHeaderValue(contract models.HeaderContract) (string, bool) {
	if contract.Example != nil {
		return fmt.Sprint(contract.Example), true
	}
	if contract.Schema != nil && contract.Schema.Projection.Example != nil {
		return fmt.Sprint(contract.Schema.Projection.Example), true
	}
	return "", false
}

func multipartRequestContentType(importedMediaType, boundary string) (string, error) {
	mediaType, parameters, err := mime.ParseMediaType(importedMediaType)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return "", errors.New("multipart request media_type must be multipart")
	}
	parameters["boundary"] = boundary
	return mime.FormatMediaType(mediaType, parameters), nil
}

func buildRawRequestBody(content *SelectedRequestRepresentation, bodyParams map[string]any) (io.Reader, error) {
	value, err := requestPayload(content, bodyParams, true, "raw request")
	if err != nil {
		return nil, err
	}
	payload, err := encodeRawPayload(strings.TrimSpace(content.PayloadParameter), value, selectedBinaryEncoding(content))
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(payload), nil
}

func requestPayload(content *SelectedRequestRepresentation, bodyParams map[string]any, requireName bool, label string) (any, error) {
	name := strings.TrimSpace(content.PayloadParameter)
	if name == "" && requireName {
		return nil, fmt.Errorf("%s payload_parameter is required", label)
	}
	if name == "" {
		return bodyParams, nil
	}
	value, ok := bodyParams[name]
	if !ok {
		return nil, fmt.Errorf("missing %s payload parameter %q", label, name)
	}
	if len(bodyParams) != 1 {
		return nil, fmt.Errorf("%s contains parameters outside payload_parameter %q", label, name)
	}
	return value, nil
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

func setRequestContentSpanAttributes(ctx context.Context, obj *models.IntegrationObject, content *SelectedRequestRepresentation, selectionOutcome string) {
	serialization, mediaFamily := requestContentTelemetryForSelection(obj, content)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("request.serialization", serialization),
		attribute.String("request.media_family", mediaFamily),
		attribute.String("request.media_selection.outcome", boundedMediaSelectionOutcome(selectionOutcome)),
		attribute.Bool("request.item_schema.present", content != nil && content.ItemSchema != nil),
		attribute.Bool("request.positional_encoding.present", content != nil && hasPositionalMultipart(content)),
	)
}

func requestContentTelemetry(obj *models.IntegrationObject) (string, string) {
	content, _, _ := SelectRequestContent(obj.RequestContent)
	return requestContentTelemetryForSelection(obj, content)
}

func requestContentTelemetryForSelection(obj *models.IntegrationObject, content *SelectedRequestRepresentation) (string, string) {
	if providerProtocol(obj) == models.ProviderProtocolGraphQL {
		return "graphql", "json"
	}
	if content == nil {
		return "none", "none"
	}
	return boundedRequestSerialization(content.Serialization), requestMediaFamily(content.MediaType)
}

func boundedMediaSelectionOutcome(value string) string {
	switch value {
	case requestMediaSelectionNone, requestMediaSelectionSingle,
		requestMediaSelectionDefault, requestMediaSelectionReject:
		return value
	default:
		return requestMediaSelectionReject
	}
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
	case mediaType == "text/event-stream":
		return "sse"
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return "json"
	case mediaType == "application/x-www-form-urlencoded":
		return "form"
	case strings.HasPrefix(mediaType, "multipart/"):
		return "multipart"
	case strings.Contains(mediaType, "xml"):
		return "xml"
	case strings.HasPrefix(mediaType, "text/"):
		return "text"
	case mediaType == "application/octet-stream", strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"):
		return "binary"
	default:
		return "other"
	}
}

func providerProtocol(obj *models.IntegrationObject) string {
	return obj.ProviderProtocol
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
