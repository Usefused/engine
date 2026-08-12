package engine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const maxRetrySignalHeaderBytes = 4096

type retryAttemptCapture struct {
	bodyReplayable      bool
	requestHeaders      map[string]bool
	responseHeaders     http.Header
	responseMediaFamily string
	errorKind           retrypolicy.ErrorKind
}

type retryAttemptCaptureKey struct{}

type providerTransportError struct{ err error }

func (e *providerTransportError) Error() string { return "provider transport failed" }
func (e *providerTransportError) Unwrap() error { return e.err }

func (d *Dispatcher) executeWithRetriesV3(ctx context.Context, span trace.Span, srv *models.Service, obj *models.IntegrationObject, params map[string]any, credentials map[string]any, bucketValues []store.BucketValue, stream ResponseStream) (int, error) {
	return d.executeRetryLoopV3(ctx, span, srv, obj, stream, func(attemptCtx context.Context, attempt int, attemptStream ResponseStream) (int, error) {
		return d.executeOnce(attemptCtx, srv, obj, params, credentials, bucketValues, attemptStream, attempt)
	})
}

type retryAttemptFunc func(context.Context, int, ResponseStream) (int, error)

// executeRetryLoopV3 defers response commitment until the first matching rule
// decides whether another provider attempt is allowed.
func (d *Dispatcher) executeRetryLoopV3(ctx context.Context, span trace.Span, srv *models.Service, obj *models.IntegrationObject, stream ResponseStream, execute retryAttemptFunc) (int, error) {
	if err := retrypolicy.Validate(srv.RetryConfig); err != nil {
		return 0, err
	}
	started := time.Now()
	var retryDeadline time.Time
	lastStatus := 0
	var lastErr error
	for attempt := 0; ; attempt++ {
		capture := newRetryAttemptCapture(srv.RetryConfig.Rules)
		attemptBase, cancel := retryAttemptContext(ctx, retryDeadline)
		attemptCtx := context.WithValue(attemptBase, retryAttemptCaptureKey{}, capture)
		attemptStream := newDeferredResponseStream(stream, func(status int) bool {
			return retryRuleCanRunAgain(srv.RetryConfig.Rules, obj, status, capture, attempt)
		})
		AddExecutionCount(ctx, "provider_attempt_count", 1)
		if resetter, ok := stream.(interface{ ResetForRetry() }); ok {
			resetter.ResetForRetry()
		}
		lastStatus, lastErr = execute(attemptCtx, attempt, attemptStream)
		cancel()
		if err := ctx.Err(); err != nil {
			// A zero-delay timer can win a select against an already-cancelled
			// parent. Stop here so cancellation can never start another provider attempt.
			recordRetryV3(span, -1, attempt, "cancelled", capture.errorKind)
			return lastStatus, err
		}
		rule, index, ok := firstRetryRule(srv.RetryConfig.Rules, obj, lastStatus, capture)
		if !ok || attempt+1 >= rule.Action.MaxAttempts {
			recordRetryV3(span, index, attempt, "stopped", capture.errorKind)
			return commitAttemptResult(attemptStream, lastStatus, normalizeError(lastErr, lastStatus))
		}
		delay := retryRuleDelay(rule.Action, capture.responseHeaders, attempt, time.Now())
		retryDeadline = earlierDeadline(retryDeadline, started.Add(time.Duration(rule.Action.MaxElapsedMs)*time.Millisecond))
		if time.Now().Add(delay).After(retryDeadline) {
			recordRetryV3(span, index, attempt, "elapsed_limit", capture.errorKind)
			return commitAttemptResult(attemptStream, lastStatus, normalizeError(lastErr, lastStatus))
		}
		recordRetryV3(span, index, attempt, "retry", capture.errorKind)
		observability.RetriesTotal.Add(ctx, 1)
		waitCtx, waitCancel := context.WithDeadline(ctx, retryDeadline)
		err := waitForRetry(waitCtx, delay)
		waitCancel()
		if err != nil {
			return lastStatus, err
		}
	}
}

func retryRuleCanRunAgain(rules []retrypolicy.Rule, obj *models.IntegrationObject, status int, capture *retryAttemptCapture, attempt int) bool {
	rule, _, ok := firstRetryRule(rules, obj, status, capture)
	return ok && attempt+1 < rule.Action.MaxAttempts
}

// firstRetryRule preserves declaration order because retry v3 defines ordered,
// first-match decisions rather than a merge of every matching predicate.
func firstRetryRule(rules []retrypolicy.Rule, obj *models.IntegrationObject, status int, capture *retryAttemptCapture) (retrypolicy.Rule, int, bool) {
	for index, rule := range rules {
		if retryRuleMatches(rule.Predicates, obj, status, capture) {
			return rule, index, true
		}
	}
	return retrypolicy.Rule{}, -1, false
}

func retryRuleMatches(p retrypolicy.Predicates, obj *models.IntegrationObject, status int, capture *retryAttemptCapture) bool {
	if !containsString(p.Methods, obj.Method) || !containsOperationKind(p.OperationKinds, retryOperationKind(obj, capture)) {
		return false
	}
	if !retryFailureMatches(p, status, capture.errorKind) || !bodyReplayabilityMatches(p.BodyReplayability, capture.bodyReplayable) {
		return false
	}
	if !idempotencyMatches(p.IdempotencyKey, capture.requestHeaders) || !requiredHeadersPresent(p.RequiredProviderHeaders, capture.responseHeaders) {
		return false
	}
	return safeRetryReplay(obj, p, capture)
}

func safeRetryReplay(obj *models.IntegrationObject, p retrypolicy.Predicates, capture *retryAttemptCapture) bool {
	if !capture.bodyReplayable {
		// HTTP method safety cannot recreate bytes from a consumed one-shot body;
		// every second provider attempt must therefore prove body replayability.
		return false
	}
	kind := retryOperationKind(obj, capture)
	unsafe := methodRequiresIdempotencyKeyForRetry(obj.Method) || kind == retrypolicy.OperationMutation
	if !unsafe {
		return true
	}
	return p.IdempotencyKey.Requirement == retrypolicy.IdempotencyKeyRequired && capture.requestHeaders[http.CanonicalHeaderKey(p.IdempotencyKey.Header)]
}

func retryOperationKind(obj *models.IntegrationObject, capture *retryAttemptCapture) retrypolicy.OperationKind {
	if capture != nil && capture.responseMediaFamily == "sse" {
		return retrypolicy.OperationStream
	}
	if capture != nil && capture.responseMediaFamily != "" {
		return providerFiniteOperationKind(obj)
	}
	return providerOperationKind(obj)
}

func retryFailureMatches(p retrypolicy.Predicates, status int, kind retrypolicy.ErrorKind) bool {
	if status > 0 {
		for _, item := range p.Statuses {
			if status >= item.Min && status <= item.Max {
				return true
			}
		}
		return false
	}
	for _, item := range p.Errors {
		if item == kind {
			return true
		}
	}
	return false
}

func providerOperationKind(obj *models.IntegrationObject) retrypolicy.OperationKind {
	if obj.OperationKind == "query" {
		return retrypolicy.OperationQuery
	}
	if obj.OperationKind == "mutation" {
		return retrypolicy.OperationMutation
	}
	if operationSuccessResponsesAreSSE(obj.Responses) {
		return retrypolicy.OperationStream
	}
	return providerFiniteOperationKind(obj)
}

func providerFiniteOperationKind(obj *models.IntegrationObject) retrypolicy.OperationKind {
	switch strings.ToUpper(obj.Method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, "QUERY":
		return retrypolicy.OperationRead
	case http.MethodDelete:
		return retrypolicy.OperationDelete
	default:
		return retrypolicy.OperationWrite
	}
}

func bodyReplayabilityMatches(required retrypolicy.BodyReplayability, replayable bool) bool {
	return required == retrypolicy.BodyAny || (required == retrypolicy.BodyReplayable && replayable) || (required == retrypolicy.BodyNotReplayable && !replayable)
}

func idempotencyMatches(predicate retrypolicy.IdempotencyKeyPredicate, headers map[string]bool) bool {
	present := predicate.Header != "" && headers[http.CanonicalHeaderKey(predicate.Header)]
	switch predicate.Requirement {
	case retrypolicy.IdempotencyKeyRequired:
		return present
	case retrypolicy.IdempotencyKeyAbsent:
		return !present
	default:
		return true
	}
}

func requiredHeadersPresent(required []string, headers http.Header) bool {
	for _, name := range required {
		if headers.Get(name) == "" {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsOperationKind(values []retrypolicy.OperationKind, target retrypolicy.OperationKind) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func retryRuleDelay(action retrypolicy.Action, headers http.Header, attempt int, now time.Time) time.Duration {
	if delay, ok := retryHeaderDelay(action.RetryAfterHeaders, headers, now); ok {
		return delay
	}
	delay := time.Duration(action.Backoff.BaseDelayMs) * time.Millisecond
	if action.Backoff.Strategy == retrypolicy.BackoffExponential {
		for step := 0; step < attempt && delay < time.Duration(action.Backoff.MaxDelayMs)*time.Millisecond; step++ {
			delay *= 2
		}
	}
	maximum := time.Duration(action.Backoff.MaxDelayMs) * time.Millisecond
	if delay > maximum {
		delay = maximum
	}
	if action.Backoff.JitterMs > 0 {
		delay += time.Duration(rand.Int64N(action.Backoff.JitterMs+1)) * time.Millisecond
	}
	return delay
}

func retryHeaderDelay(config []retrypolicy.RetryAfterHeader, headers http.Header, now time.Time) (time.Duration, bool) {
	for _, candidate := range config {
		raw := headers.Get(candidate.Name)
		for _, format := range candidate.Formats {
			when, err := parseResetHeader(raw, string(format), now)
			if raw == "" || err != nil {
				continue
			}
			delay := when.Sub(now)
			maximum := time.Duration(candidate.MaxDelayMs) * time.Millisecond
			if delay > maximum {
				delay = maximum
			}
			return maxDuration(delay, 0), true
		}
	}
	return 0, false
}

func recordRetryRequest(ctx context.Context, request *http.Request) {
	capture, _ := ctx.Value(retryAttemptCaptureKey{}).(*retryAttemptCapture)
	if capture == nil {
		return
	}
	capture.bodyReplayable = requestBodyReplayable(request)
	for name := range capture.requestHeaders {
		capture.requestHeaders[name] = request.Header.Get(name) != ""
	}
}

func requestBodyReplayable(request *http.Request) bool {
	if request == nil || request.Body == nil || request.Body == http.NoBody {
		return true
	}
	return request.GetBody != nil
}

func recordRetryResponse(ctx context.Context, headers http.Header) {
	capture, _ := ctx.Value(retryAttemptCaptureKey{}).(*retryAttemptCapture)
	if capture == nil {
		return
	}
	for name := range capture.responseHeaders {
		value := headers.Get(name)
		if len(value) <= maxRetrySignalHeaderBytes {
			capture.responseHeaders.Set(name, value)
		}
	}
}

func recordRetryResponseMedia(ctx context.Context, family string) {
	capture, _ := ctx.Value(retryAttemptCaptureKey{}).(*retryAttemptCapture)
	if capture != nil {
		capture.responseMediaFamily = family
	}
}

func recordRetryTransportError(ctx context.Context, err error) {
	capture, _ := ctx.Value(retryAttemptCaptureKey{}).(*retryAttemptCapture)
	if capture != nil {
		capture.errorKind = classifyRetryError(err)
	}
}

func recordRetryChallengeTransportError(ctx context.Context, err error) {
	var transport *providerTransportError
	if errors.As(err, &transport) {
		recordRetryTransportError(ctx, transport.err)
	}
}

func classifyRetryError(err error) retrypolicy.ErrorKind {
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsTemporary {
		return retrypolicy.ErrorTemporaryDNS
	}
	if isTLSRetryError(err) {
		return retrypolicy.ErrorTLSHandshake
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return retrypolicy.ErrorConnectionReset
	}
	if timeoutKind, ok := classifyTimeout(err); ok {
		return timeoutKind
	}
	return retrypolicy.ErrorTransport
}

func isTLSRetryError(err error) bool {
	var tlsErr tls.RecordHeaderError
	if errors.As(err, &tlsErr) {
		return true
	}
	var authority x509.UnknownAuthorityError
	var certificate x509.CertificateInvalidError
	return errors.As(err, &authority) || errors.As(err, &certificate)
}

func classifyTimeout(err error) (retrypolicy.ErrorKind, bool) {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		if opErr.Op == "read" {
			return retrypolicy.ErrorReadTimeout, true
		}
		return retrypolicy.ErrorConnectTimeout, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return retrypolicy.ErrorReadTimeout, true
	}
	return "", false
}

func newRetryAttemptCapture(rules []retrypolicy.Rule) *retryAttemptCapture {
	capture := &retryAttemptCapture{requestHeaders: make(map[string]bool), responseHeaders: make(http.Header)}
	for _, rule := range rules {
		if name := rule.Predicates.IdempotencyKey.Header; name != "" {
			capture.requestHeaders[http.CanonicalHeaderKey(name)] = false
		}
		for _, name := range rule.Predicates.RequiredProviderHeaders {
			capture.responseHeaders[http.CanonicalHeaderKey(name)] = nil
		}
		for _, header := range rule.Action.RetryAfterHeaders {
			capture.responseHeaders[http.CanonicalHeaderKey(header.Name)] = nil
		}
	}
	return capture
}

func retryAttemptContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

func earlierDeadline(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func recordRetryV3(span trace.Span, rule, attempt int, outcome string, kind retrypolicy.ErrorKind) {
	span.AddEvent("engine.retry.decision", trace.WithAttributes(attribute.Int("retry.rule_index", rule), attribute.Int("retry.attempt", attempt+1), attribute.String("retry.outcome", outcome), attribute.String("retry.error_kind", string(kind))))
}
