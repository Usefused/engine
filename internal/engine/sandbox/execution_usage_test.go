package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/trace/noop"
)

type captureExecutionUsageRecorder struct {
	increments []models.EngineUsageIncrement
}

func (r *captureExecutionUsageRecorder) Record(increment models.EngineUsageIncrement) {
	r.increments = append(r.increments, increment)
}

func TestRecordEngineExecutionUsageCapturesAggregateOnlyMetrics(t *testing.T) {
	orig := globalExecutionUsageRecorder
	recorder := &captureExecutionUsageRecorder{}
	SetExecutionUsageRecorder(recorder)
	defer SetExecutionUsageRecorder(orig)

	span := noop.NewTracerProvider().Tracer("test").Start
	ctx, testSpan := span(context.Background(), "execution")
	recordEngineExecutionUsage(ctx, testSpan, executionAuditState{startedAt: time.Now()}, errors.New("provider failed"))
	testSpan.End()

	if len(recorder.increments) != 2 {
		t.Fatalf("expected total+failed increments, got %#v", recorder.increments)
	}
	if recorder.increments[0].Metric != models.EngineUsageMetricExecutionTotal ||
		recorder.increments[1].Metric != models.EngineUsageMetricExecutionFailed {
		t.Fatalf("unexpected metrics: %#v", recorder.increments)
	}
	for _, increment := range recorder.increments {
		if increment.BucketSeconds != usageBucketSeconds || increment.Count != 1 {
			t.Fatalf("unexpected aggregate shape: %#v", increment)
		}
	}
}

func TestRecordEngineExecutionUsageCountsProviderErrorResponseAsFailed(t *testing.T) {
	recorder := &captureExecutionUsageRecorder{}
	SetExecutionUsageRecorder(recorder)
	defer SetExecutionUsageRecorder(nil)

	ctx, span := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "execution")
	recordEngineExecutionUsage(ctx, span, executionAuditState{
		startedAt: time.Now(), providerHTTPStatus: 429,
	}, nil)
	span.End()

	if len(recorder.increments) != 2 || recorder.increments[1].Metric != models.EngineUsageMetricExecutionFailed {
		t.Fatalf("unexpected usage increments: %#v", recorder.increments)
	}
}
