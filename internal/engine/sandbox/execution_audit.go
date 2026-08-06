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
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
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

func contextWithExecutionTransport(ctx context.Context, transport string) context.Context {
	return context.WithValue(ctx, executionTransportContextKey{}, transport)
}

func executionTransportFromContext(ctx context.Context) string {
	if transport, ok := ctx.Value(executionTransportContextKey{}).(string); ok && transport != "" {
		return transport
	}
	return models.EngineExecutionTransportSDK
}

func recordEngineExecutionAudit(ctx context.Context, span trace.Span, state executionAuditState, execErr error) {
	event := models.EngineExecutionEvent{
		ID:                  uuid.New(),
		AccountID:           state.identity.AccountID,
		AppFamilyID:         state.identity.AppFamilyID,
		AppID:               state.identity.AppID,
		AppVersion:          state.identity.AppVersion,
		Transport:           executionTransportFromContext(ctx),
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
	attachExecutionTimings(ctx, &event)
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

func classifyExecutionFailure(execErr error, status int) (string, string) {
	if errors.Is(execErr, context.DeadlineExceeded) {
		return "timeout", "deadline_exceeded"
	}
	if errors.Is(execErr, context.Canceled) {
		return "engine", "cancelled"
	}
	if status >= http.StatusBadRequest {
		return classifyExecutionFailureByStatus("", status)
	}
	if execErr == nil {
		return "", ""
	}
	return classifyExecutionFailureByStatus(execErr.Error(), status)
}

func executionFailed(execErr error, status int) bool {
	return execErr != nil || status >= http.StatusBadRequest
}

func executionFailureReason(execErr error, status int) string {
	if execErr != nil {
		return execErr.Error()
	}
	return providerStatusError(status)
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
	return match.endpoint.Method
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
