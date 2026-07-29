package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

type executionTransportContextKey struct{}

type executionAuditRecorder interface {
	Record(models.EngineExecutionEvent)
}

type executionAuditState struct {
	artifactID          uuid.UUID
	endpointName        string
	startedAt           time.Time
	match               *scopedEndpoint
	selectedEnvironment string
	// idempotencyReplayed is true when this execution was served from the
	// idempotency cache instead of dispatching to the vendor.
	idempotencyReplayed bool
}

var globalExecutionAuditRecorder executionAuditRecorder

func SetExecutionAuditRecorder(recorder executionAuditRecorder) {
	globalExecutionAuditRecorder = recorder
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
	if globalExecutionAuditRecorder == nil {
		return
	}
	event := models.EngineExecutionEvent{
		ID:                  uuid.New(),
		ArtifactID:          state.artifactID,
		Transport:           executionTransportFromContext(ctx),
		EndpointName:        state.endpointName,
		Environment:         state.selectedEnvironment,
		Status:              models.EngineExecutionStatusSuccess,
		StartedAt:           state.startedAt,
		EndedAt:             time.Now(),
		CreatedAt:           time.Now(),
		IdempotencyKeyHash:  hashExecutionValue(idempotencyKeyFromContext(ctx)),
		RequestBodyHash:     requestBodyHashFromContext(ctx),
		IdempotencyReplayed: state.idempotencyReplayed,
	}
	event.LatencyMs = event.EndedAt.Sub(event.StartedAt).Milliseconds()
	if execErr != nil {
		event.Status = models.EngineExecutionStatusFailed
		event.FailureReason = execErr.Error()
	}
	if state.match != nil {
		event.ServiceID = state.match.service.ID
		event.ServiceVersionID = state.match.serviceVersionID
	}
	if spanContext := span.SpanContext(); spanContext.IsValid() {
		event.TraceID = spanContext.TraceID().String()
		event.SpanID = spanContext.SpanID().String()
	}
	attachExecutionTimings(ctx, &event)
	globalExecutionAuditRecorder.Record(event)
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
