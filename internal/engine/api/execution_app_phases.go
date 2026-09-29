package api

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordExecutionPhases gives OTEL and the canonical receipt the same bounded measurements.
func (host *recordingCapabilityHost) RecordExecutionPhases(ctx context.Context, phases []executionappvm.PhaseTiming) {
	// Invalid worker metadata cannot enter either observability projection.
	if !executionappvm.ValidExecutionPhases(phases) {
		return
	}
	host.mu.Lock()
	// Completion is write-once so duplicate frames cannot create duplicate span evidence.
	if host.phases != nil {
		host.mu.Unlock()
		return
	}
	host.phases = append([]executionappvm.PhaseTiming{}, phases...)
	host.mu.Unlock()
	for _, phase := range phases {
		_, span := otel.Tracer("engine").Start(ctx, "engine.unified_app."+phase.Name, trace.WithTimestamp(phase.StartedAt))
		// Raw exceptions remain private even when a stage fails before any provider call.
		if phase.Failed {
			span.SetStatus(codes.Error, "execution_failed")
		} else {
			span.SetStatus(codes.Ok, "completed")
		}
		span.End(trace.WithTimestamp(phase.StartedAt.Add(phase.Duration)))
	}
}

// phaseTimings persists durations through the existing execution-event worker, independent of exporter sampling.
func (host *recordingCapabilityHost) phaseTimings() json.RawMessage {
	host.mu.Lock()
	defer host.mu.Unlock()
	// Historical and interrupted workers have no trustworthy phase evidence to display.
	if len(host.phases) == 0 {
		return nil
	}
	timings := make(map[string]float64, len(host.phases))
	for _, phase := range host.phases {
		timings["unified_app_"+phase.Name] = float64(phase.Duration) / 1e6
	}
	raw, _ := json.Marshal(timings)
	return raw
}
