package sandbox

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const usageBucketSeconds = 60

type executionUsageRecorder interface {
	Record(models.EngineUsageIncrement)
}

var globalExecutionUsageRecorder executionUsageRecorder

func SetExecutionUsageRecorder(recorder executionUsageRecorder) {
	globalExecutionUsageRecorder = recorder
}

func recordEngineExecutionUsage(ctx context.Context, span trace.Span, state executionAuditState, execErr error) {
	if globalExecutionUsageRecorder == nil {
		return
	}
	bucketStart := state.startedAt.UTC().Truncate(time.Minute)
	for _, metric := range executionUsageMetrics(execErr) {
		globalExecutionUsageRecorder.Record(models.EngineUsageIncrement{
			Metric:        metric,
			BucketStart:   bucketStart,
			BucketSeconds: usageBucketSeconds,
			Count:         1,
		})
	}
	// The span event is intentionally aggregate-only. Users need to debug that
	// an execution changed commercial accounting state, but payloads, headers,
	// URLs, and credentials never belong in this signal.
	span.AddEvent("engine.usage_counters_enqueued", trace.WithAttributes(
		attribute.String("usage.bucket_start", bucketStart.Format(time.RFC3339)),
		attribute.String("usage.status", executionUsageStatus(execErr)),
	))
}

func executionUsageMetrics(execErr error) []string {
	if execErr != nil {
		return []string{models.EngineUsageMetricExecutionTotal, models.EngineUsageMetricExecutionFailed}
	}
	return []string{models.EngineUsageMetricExecutionTotal, models.EngineUsageMetricExecutionSuccess}
}

func executionUsageStatus(execErr error) string {
	if execErr != nil {
		return models.EngineExecutionStatusFailed
	}
	return models.EngineExecutionStatusSuccess
}
