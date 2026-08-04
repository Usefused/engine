package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

type executionTimingsKey struct{}

// ExecutionTimings accumulates one Engine runtime call's internal timings.
// It is context-carried so the gRPC edge, scope resolver, and dispatcher can
// contribute measurements without coupling those layers to one another.
type ExecutionTimings struct {
	mu      sync.Mutex
	entries map[string]time.Duration
	counts  map[string]int64
}

func NewExecutionTimings() *ExecutionTimings {
	return &ExecutionTimings{entries: make(map[string]time.Duration), counts: make(map[string]int64)}
}

func ContextWithExecutionTimings(ctx context.Context, timings *ExecutionTimings) context.Context {
	if timings == nil {
		return ctx
	}
	return context.WithValue(ctx, executionTimingsKey{}, timings)
}

func ExecutionTimingsFromContext(ctx context.Context) (*ExecutionTimings, bool) {
	timings, ok := ctx.Value(executionTimingsKey{}).(*ExecutionTimings)
	return timings, ok && timings != nil
}

func RecordExecutionTiming(ctx context.Context, name string, duration time.Duration) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.Record(name, duration)
	}
}

func MeasureExecutionTiming(ctx context.Context, name string, started time.Time) {
	RecordExecutionTiming(ctx, name, time.Since(started))
}

func (t *ExecutionTimings) Record(name string, duration time.Duration) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[name] = duration
}

func (t *ExecutionTimings) Add(name string, duration time.Duration) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[name] += duration
}

func AddExecutionTiming(ctx context.Context, name string, duration time.Duration) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.Add(name, duration)
	}
}

func RecordExecutionCount(ctx context.Context, name string, value int64) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.RecordCount(name, value)
	}
}

func (t *ExecutionTimings) RecordCount(name string, value int64) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[name] = value
}

func (t *ExecutionTimings) Count(name string) int64 {
	if t == nil || name == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[name]
}

func (t *ExecutionTimings) SnapshotMilliseconds() map[string]float64 {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]float64, len(t.entries))
	for k, v := range t.entries {
		out[k] = float64(v.Microseconds()) / 1000
	}
	return out
}

func (t *ExecutionTimings) Attributes() []attribute.KeyValue {
	snapshot := t.SnapshotMilliseconds()
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	attrs := make([]attribute.KeyValue, 0, len(keys))
	for _, key := range keys {
		attrs = append(attrs, attribute.Float64(fmt.Sprintf("engine.timing.%s_ms", key), snapshot[key]))
	}
	return attrs
}
