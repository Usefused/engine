package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type executionTransportContextKey struct{}

type executionAuditState struct {
	identity auth.RuntimeIdentity
	// accountID identifies which account this execution belongs to. It must
	// be threaded through from resolveExecutionIdentity's return value at the
	// call site -- without it, recordEngineExecutionAudit has no way to
	// populate models.EngineExecutionEvent.AccountID, and every published
	// event ends up persisted with a NULL account_id. That makes the event
	// exist in fused_engine_execution_events but permanently invisible to
	// GetWorkspaceExecutionAnalytics, which filters on the caller's real
	// account ID -- the Activity page then shows "No calls" no matter how
	// many executions actually succeeded.
	endpointName        string
	startedAt           time.Time
	match               *scopedEndpoint
	selectedEnvironment string
	environmentSource   string
	providerHost        string
	providerHTTPStatus  int
	// idempotencyReplayed is true when this execution was served from the
	// idempotency cache instead of dispatching to the vendor.
	idempotencyReplayed bool
}

// contextWithExecutionTransport attaches one server-owned low-cardinality
// execution ingress label to every physical receipt emitted below it.
func contextWithExecutionTransport(ctx context.Context, transport string) context.Context {
	return context.WithValue(ctx, executionTransportContextKey{}, transport)
}

// ContextWithRESTExecutionTransport marks an in-process REST execution without
// exposing the generic transport setter outside the sandbox package.
func ContextWithRESTExecutionTransport(ctx context.Context) context.Context {
	return contextWithExecutionTransport(ctx, models.EngineExecutionTransportREST)
}

// ContextWithMCPExecutionTransport marks only Engine-owned MCP adapter calls;
// model-authored parameters cannot construct Go context values.
func ContextWithMCPExecutionTransport(ctx context.Context) context.Context {
	return contextWithExecutionTransport(ctx, models.EngineExecutionTransportMCP)
}

// ExecutionTransportFromContext defaults older in-process callers to SDK while
// preserving explicit MCP and REST ingress labels.
func ExecutionTransportFromContext(ctx context.Context) string {
	if transport, ok := ctx.Value(executionTransportContextKey{}).(string); ok && transport != "" {
		return transport
	}
	return models.EngineExecutionTransportSDK
}

// recordEngineExecutionAudit publishes one physical receipt with optional server-owned Unified correlation.
func recordEngineExecutionAudit(ctx context.Context, span trace.Span, state executionAuditState, execErr error) {
	event := models.EngineExecutionEvent{
		ID:                  uuid.New(),
		AccountID:           state.identity.AccountID,
		AppFamilyID:         state.identity.AppFamilyID,
		AppID:               state.identity.AppID,
		AppTokenID:          state.identity.TokenID,
		AppVersion:          state.identity.AppVersion,
		Transport:           ExecutionTransportFromContext(ctx),
		Direction:           models.EngineExecutionDirectionOutbound,
		EndpointName:        state.endpointName,
		HTTPMethod:          executionHTTPMethod(state.match),
		RequestPath:         executionRequestPath(state.match),
		Environment:         state.selectedEnvironment,
		EnvironmentSource:   state.environmentSource,
		ProviderHost:        state.providerHost,
		Status:              models.EngineExecutionStatusSuccess,
		StartedAt:           state.startedAt,
		EndedAt:             time.Now(),
		CreatedAt:           time.Now(),
		IdempotencyKeyHash:  hashExecutionValue(idempotencyKeyFromContext(ctx)),
		RequestBodyHash:     requestBodyHashFromContext(ctx),
		IdempotencyReplayed: state.idempotencyReplayed,
		AttemptCount:        1,
	}
	if state.providerHTTPStatus > 0 {
		event.ProviderHTTPStatus = &state.providerHTTPStatus
	}
	event.LatencyMs = event.EndedAt.Sub(event.StartedAt).Milliseconds()
	if executionFailed(execErr, state.providerHTTPStatus) {
		event.Status = models.EngineExecutionStatusFailed
		event.FailureReason = executionFailureReason(execErr, state.providerHTTPStatus)
	}
	if state.match != nil {
		event.ServiceID = state.match.service.ID
		event.ServiceVersionID = state.match.serviceVersionID
		event.OperationID = state.match.endpoint.ID
		event.ProviderProtocol = effectiveProviderProtocol(state.match.endpoint)
	}
	event.ProviderStatusClass = providerStatusClass(state.providerHTTPStatus, execErr)
	event.FailureCategory, event.FailureCode = classifyExecutionFailure(execErr, state.providerHTTPStatus)
	span.SetAttributes(
		attribute.String("execution.outcome", event.Status),
		attribute.String("execution.failure_category", event.FailureCategory),
		attribute.String("execution.failure_code", event.FailureCode),
	)
	if spanContext := span.SpanContext(); spanContext.IsValid() {
		event.TraceID = spanContext.TraceID().String()
		event.SpanID = spanContext.SpanID().String()
	}
	executionevent.AttachUnifiedChild(ctx, &event)
	attachExecutionTimings(ctx, &event)
	if event.PaginationType != "" {
		span.SetAttributes(
			attribute.String("pagination.type", event.PaginationType),
			attribute.Int64("pagination.page_count", event.PaginationPageCount),
			attribute.Int64("pagination.item_count", event.PaginationItemCount),
			attribute.Int64("pagination.byte_count", event.PaginationByteCount),
			attribute.String("pagination.stop_reason", event.PaginationStopReason),
		)
	}
	if event.RateLimitDecision != "" {
		span.SetAttributes(
			attribute.String("rate_limit.decision", event.RateLimitDecision),
			attribute.Int64("rate_limit.policy_count", event.RateLimitPolicyCount),
			attribute.StringSlice("rate_limit.scope_kinds", event.RateLimitScopeKinds),
			attribute.StringSlice("rate_limit.units", event.RateLimitUnits),
			attribute.Int64Slice("rate_limit.unit_totals", event.RateLimitUnitTotals),
			attribute.String("rate_limit.retry_outcome", event.RateLimitRetryOutcome),
			attribute.String("rate_limit.header_outcome", event.RateLimitHeaderOutcome),
		)
	}
	if err := executionevent.Publish(ctx, event); err != nil {
		slog.ErrorContext(ctx, "Failed to publish execution event", slog.Any("error", err), slog.String("event_id", event.ID.String()))
	}
}

func providerStatusClass(status int, execErr error) string {
	if status > 0 {
		return fmt.Sprintf("%dxx", status/100)
	}
	if execErr != nil {
		return "network"
	}
	return "none"
}

// classifyExecutionFailure maps execution evidence to bounded durable and OTEL dimensions without retaining raw errors.
func classifyExecutionFailure(execErr error, status int) (string, string) {
	if _, incompatible := fusedobject.ExecutionContractCompatibilityDetails(execErr); incompatible {
		return "contract", fusedobject.ExecutionCapabilityRequiredCode
	}
	// Caller pagination-policy decisions are already typed, so preserve their bounded reason instead of falling through to a network failure.
	if code, ok := paginationIntentFailureCode(execErr); ok {
		return "pagination", code
	}
	if code := engine.PaginationFailureCode(execErr); code != "" {
		return "pagination", code
	}
	if errors.Is(execErr, context.DeadlineExceeded) {
		return "timeout", "deadline_exceeded"
	}
	if errors.Is(execErr, context.Canceled) {
		return "engine", "cancelled"
	}
	if isTransportExecutionFailure(execErr) {
		return "network", "request_failed"
	}
	if status >= http.StatusBadRequest {
		return classifyExecutionFailureByStatus("", status)
	}
	if execErr == nil {
		return "", ""
	}
	return classifyExecutionFailureByStatus(execErr.Error(), status)
}

// paginationIntentFailureCode maps only the closed Engine reason vocabulary into durable and OTEL-safe codes.
func paginationIntentFailureCode(execErr error) (string, bool) {
	var validationErr *engine.PaginationIntentValidationError
	// Untyped failures continue through the existing status and transport classifiers.
	if !errors.As(execErr, &validationErr) {
		return "", false
	}
	// A typed nil or future reason must remain bounded rather than emitting caller-controlled text.
	if validationErr == nil {
		return "intent_invalid", true
	}
	// Explicit mapping prevents a newly added reason from silently becoming an unbounded telemetry dimension.
	switch validationErr.Reason {
	case engine.PaginationIntentInvalidValue:
		// Global bound failures remain distinct from operation-policy decisions.
		return "intent_invalid_value", true
	case engine.PaginationIntentNotSupported:
		// Unsupported controls identify a non-paginated resolved operation without exposing its contract.
		return "intent_not_supported", true
	case engine.PaginationIntentBoundNotLower:
		// Non-tightening controls retain their policy decision without recording the requested or effective bound.
		return "intent_bound_not_lower", true
	default:
		// Future typed reasons collapse to one safe code until this allowlist is deliberately extended.
		return "intent_invalid", true
	}
}

func isTransportExecutionFailure(execErr error) bool {
	var transport *url.Error
	return errors.As(execErr, &transport)
}

func executionFailed(execErr error, status int) bool {
	return execErr != nil || status >= http.StatusBadRequest
}

func executionFailureReason(execErr error, status int) string {
	// Durable receipts carry the same closed classification as OTEL. Raw
	// transport errors can embed URLs, query values, headers, or credentials.
	return executionFailureDescription(execErr, status)
}

func classifyExecutionFailureByStatus(message string, status int) (string, string) {
	message = strings.ToLower(message)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth", "provider_auth"
	case status == http.StatusTooManyRequests:
		return "rate_limit", "provider_rate_limit"
	case status >= http.StatusInternalServerError:
		return "provider", "provider_error"
	case strings.Contains(message, "scopeerror") || strings.Contains(message, "unauthorized access"):
		return "policy", "scope_denied"
	case strings.Contains(message, "credential") || strings.Contains(message, "auth"):
		return "auth", "credential_error"
	case status >= http.StatusBadRequest:
		return "validation", "provider_rejected"
	default:
		return "network", "request_failed"
	}
}

func executionHTTPMethod(match *scopedEndpoint) string {
	if match == nil {
		return ""
	}
	switch strings.ToUpper(match.endpoint.Method) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, "QUERY", "SOAP":
		return strings.ToUpper(match.endpoint.Method)
	default:
		return "CUSTOM"
	}
}

func executionRequestPath(match *scopedEndpoint) string {
	if match == nil {
		return ""
	}
	if match.endpoint.NormalizedPath != "" {
		return match.endpoint.NormalizedPath
	}
	return match.endpoint.Path
}

func attachExecutionTimings(ctx context.Context, event *models.EngineExecutionEvent) {
	timings, ok := engine.ExecutionTimingsFromContext(ctx)
	if !ok {
		return
	}
	snapshot := timings.SnapshotMilliseconds()
	pagination := timings.PaginationSummary()
	authSummary := timings.AuthSummary()
	rateLimit := timings.RateLimitSummary()
	event.AuthSchemeNames = authSummary.SchemeNames
	event.AuthSchemeTypes = authSummary.SchemeTypes
	event.AuthSchemeCount = authSummary.SchemeCount
	event.AuthSelectionOutcome = authSummary.Outcome
	event.PaginationType = pagination.Type
	event.PaginationPageCount = pagination.PageCount
	event.PaginationItemCount = pagination.ItemCount
	event.PaginationByteCount = pagination.ByteCount
	event.PaginationStopReason = pagination.StopReason
	event.RateLimitDecision = rateLimit.Decision
	event.RateLimitPolicyCount = rateLimit.PolicyCount
	event.RateLimitScopeKinds = rateLimit.ScopeKinds
	event.RateLimitUnits = rateLimit.Units
	event.RateLimitUnitTotals = rateLimit.UnitTotals
	event.RateLimitRetryOutcome = rateLimit.RetryOutcome
	event.RateLimitHeaderOutcome = rateLimit.HeaderOutcome
	if len(snapshot) == 0 {
		return
	}
	if providerMs, ok := snapshot["provider_total"]; ok {
		value := int64(providerMs)
		event.ProviderLatencyMs = &value
	}
	if attempts := timings.Count("provider_attempt_count"); attempts > 0 {
		event.AttemptCount = int(attempts)
	}
	if encoded, err := json.Marshal(snapshot); err == nil {
		event.Timings = encoded
	}
}

func hashExecutionValue(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
