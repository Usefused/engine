package api

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// telemetryFailurePhase prevents authored diagnostic strings from becoming unbounded or sensitive trace metadata.
func telemetryFailurePhase(phase string) string {
	// Only Engine-defined phases may cross from private worker diagnostics into the exporter.
	switch phase {
	case "input_validation", "execute", "output_validation", "compilation", "initialization", "timeout":
		return phase
	default:
		return "runtime"
	}
}

// finishUnifiedAppSpan records durable outcomes without exporting payloads, read handles, or exception messages.
func finishUnifiedAppSpan(span trace.Span, result capabilityExecutionEnvelope, requestErr *restExecutionError) {
	// Admission/storage failures have no trustworthy terminal result and receive a fixed outcome.
	if requestErr != nil {
		span.SetStatus(codes.Error, "execution_unavailable")
		return
	}
	// Status derives from Engine persistence, but is still allowlisted at the telemetry boundary.
	switch result.Status {
	case "succeeded":
		span.SetStatus(codes.Ok, "succeeded")
	case "queued", "running":
		span.SetAttributes(attribute.String("execution.status", result.Status))
	default:
		span.SetStatus(codes.Error, "execution_failed")
	}
}
