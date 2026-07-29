package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

type captureExecutionAuditRecorder struct {
	event models.EngineExecutionEvent
}

func (r *captureExecutionAuditRecorder) Record(event models.EngineExecutionEvent) {
	r.event = event
}

func TestRecordEngineExecutionAuditCapturesCompactSafeReceipt(t *testing.T) {
	orig := globalExecutionAuditRecorder
	recorder := &captureExecutionAuditRecorder{}
	SetExecutionAuditRecorder(recorder)
	defer SetExecutionAuditRecorder(orig)

	timings := engine.NewExecutionTimings()
	ctx := engine.ContextWithExecutionTimings(context.Background(), timings)
	ctx = contextWithExecutionIdentity(ctx, "raw-idempotency-key", "request-body-hash")
	ctx = contextWithExecutionTransport(ctx, models.EngineExecutionTransportSDK)
	engine.RecordExecutionTiming(ctx, "provider_total", 12*time.Millisecond)

	serviceID := uuid.New()
	artifactID := uuid.New()
	startedAt := time.Now().Add(-25 * time.Millisecond)
	recordEngineExecutionAudit(ctx, trace.SpanFromContext(ctx), executionAuditState{
		artifactID:   artifactID,
		endpointName: "repos.list",
		startedAt:    startedAt,
		match: &scopedEndpoint{
			service:          &fusedobject.ServiceMetadata{ID: serviceID},
			serviceVersionID: "version-1",
		},
		selectedEnvironment: "production",
	}, errors.New("provider failed"))

	event := recorder.event
	if event.ArtifactID != artifactID || event.ServiceID != serviceID {
		t.Fatalf("unexpected receipt ids: sdk=%s service=%s", event.ArtifactID, event.ServiceID)
	}
	if event.Status != models.EngineExecutionStatusFailed {
		t.Fatalf("status = %q, want failed", event.Status)
	}
	if event.IdempotencyKeyHash == "" || event.IdempotencyKeyHash == "raw-idempotency-key" {
		t.Fatal("idempotency key must be hashed before persistence")
	}
	if event.RequestBodyHash != "request-body-hash" {
		t.Fatalf("request body hash = %q", event.RequestBodyHash)
	}
	if event.ProviderLatencyMs == nil || *event.ProviderLatencyMs != 12 {
		t.Fatalf("provider latency = %v, want 12ms", event.ProviderLatencyMs)
	}
	if len(event.Timings) == 0 {
		t.Fatal("expected timing summary JSON to be persisted")
	}
}
