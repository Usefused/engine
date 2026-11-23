package observability

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// TraceIDFromContext returns the active OpenTelemetry trace identifier or an
// empty string so audit records never persist placeholder all-zero trace IDs.
func TraceIDFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	// Requests without a valid trace span must omit the trace identifier.
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}
