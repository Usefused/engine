package api

import (
	"context"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type controlMutationTelemetryRecordedKey struct{}

// writeControlAPIError emits the shared structured control-plane error envelope
// so CLI and UI callers receive one stable, secret-safe diagnostic contract.
func writeControlAPIError(w http.ResponseWriter, ctx context.Context, status int, code, message, remediation string) {
	writeWorkspaceConfigError(w, workspaceConfigHTTPError{
		status: status, code: code, message: message, remediation: remediation,
	}, ctx)
}

// writeControlAPIMutationError adds only caller-proven mutation state to the
// shared envelope so failures never imply a commit outcome the handler cannot prove.
func writeControlAPIMutationError(w http.ResponseWriter, ctx context.Context, status int, code, message, remediation, phase, operationID, commitState, recovery string) {
	writeWorkspaceConfigError(w, workspaceConfigHTTPError{
		status: status, code: code, message: message, remediation: remediation,
		phase: phase, operationID: operationID, commitState: commitState, recovery: recovery,
	}, ctx)
}

// recordControlMutationFailure annotates the request's existing span with only
// the stable diagnosis and proven mutation state already admitted for HTTP.
func recordControlMutationFailure(err error, contexts []context.Context) {
	var httpErr workspaceConfigHTTPError
	// Read-only and untyped failures have no mutation proof to project.
	if !errors.As(err, &httpErr) || httpErr.phase == "" {
		return
	}
	for _, ctx := range contexts {
		// Specialized lifecycle spans own their exact reviewed attribute allowlist.
		if controlMutationTelemetryAlreadyRecorded(ctx) {
			return
		}
		// Only the first live request span owns this mutation outcome.
		if ctx == nil || !trace.SpanContextFromContext(ctx).IsValid() {
			continue
		}
		code := workspaceConfigErrorCode(httpErr)
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(
			attribute.String("control.mutation.outcome", "failed"),
			attribute.String("control.mutation.error_code", code),
			attribute.String("control.mutation.phase", httpErr.phase),
			attribute.String("control.mutation.commit_state", httpErr.commitState),
		)
		span.SetStatus(codes.Error, code)
		return
	}
}

// contextWithControlMutationTelemetryRecorded marks a handler whose existing
// specialized helper already owns its exact outcome and error allowlist.
func contextWithControlMutationTelemetryRecorded(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlMutationTelemetryRecordedKey{}, true)
}

// controlMutationTelemetryAlreadyRecorded prevents the shared writer from
// adding a second attribute vocabulary to a specialized mutation span.
func controlMutationTelemetryAlreadyRecorded(ctx context.Context) bool {
	// Nil contexts cannot carry the specialized-telemetry marker.
	if ctx == nil {
		return false
	}
	recorded, _ := ctx.Value(controlMutationTelemetryRecordedKey{}).(bool)
	return recorded
}
