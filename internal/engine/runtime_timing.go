package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

type executionTimingsKey struct{}

// ExecutionTimings accumulates one Engine runtime call's internal timings.
// It is context-carried so the gRPC edge, scope resolver, and dispatcher can
// contribute measurements without coupling those layers to one another.
type ExecutionTimings struct {
	mu         sync.Mutex
	entries    map[string]time.Duration
	counts     map[string]int64
	pagination PaginationExecutionSummary
	auth       AuthExecutionSummary
	rateLimit  RateLimitExecutionSummary
}

// PaginationExecutionSummary is the bounded execution receipt shared by OTEL
// and Activity. Continuation values and provider response metadata never enter it.
type PaginationExecutionSummary struct {
	Type       string
	PageCount  int64
	ItemCount  int64
	ByteCount  int64
	StopReason string
}

type AuthExecutionSummary struct {
	SchemeNames []string
	SchemeTypes []string
	SchemeCount int64
	Outcome     string
}

type RateLimitExecutionSummary struct {
	Decision        string
	PolicyCount     int64
	ScopeKinds      []string
	Units           []string
	UnitTotals      []int64
	RetryOutcome    string
	HeaderOutcome   string
	ObservedDenials int64
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

func AddExecutionCount(ctx context.Context, name string, value int64) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.AddCount(name, value)
	}
}

func (t *ExecutionTimings) AddCount(name string, value int64) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[name] += value
}

func RecordPaginationSummary(ctx context.Context, summary PaginationExecutionSummary) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.RecordPagination(summary)
	}
}

func (t *ExecutionTimings) RecordPagination(summary PaginationExecutionSummary) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pagination = summary
}

func (t *ExecutionTimings) PaginationSummary() PaginationExecutionSummary {
	if t == nil {
		return PaginationExecutionSummary{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pagination
}

func RecordAuthSummary(ctx context.Context, summary AuthExecutionSummary) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.RecordAuth(summary)
	}
}

func (t *ExecutionTimings) RecordAuth(summary AuthExecutionSummary) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	summary.SchemeNames = append([]string(nil), summary.SchemeNames...)
	summary.SchemeTypes = append([]string(nil), summary.SchemeTypes...)
	t.auth = summary
}

func (t *ExecutionTimings) AuthSummary() AuthExecutionSummary {
	if t == nil {
		return AuthExecutionSummary{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	summary := t.auth
	summary.SchemeNames = append([]string(nil), summary.SchemeNames...)
	summary.SchemeTypes = append([]string(nil), summary.SchemeTypes...)
	return summary
}

func RecordRateLimitSummary(ctx context.Context, summary RateLimitExecutionSummary) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.RecordRateLimit(summary)
	}
}

func (t *ExecutionTimings) RecordRateLimit(summary RateLimitExecutionSummary) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	summary = mergeRateLimitSummary(t.rateLimit, summary)
	if summary.HeaderOutcome == "" {
		summary.HeaderOutcome = t.rateLimit.HeaderOutcome
	}
	t.rateLimit = summary
}

func RecordRateLimitHeaderOutcome(ctx context.Context, outcome string) {
	if timings, ok := ExecutionTimingsFromContext(ctx); ok {
		timings.mu.Lock()
		timings.rateLimit.HeaderOutcome = mergeRateLimitHeaderOutcome(timings.rateLimit.HeaderOutcome, outcome)
		timings.mu.Unlock()
	}
}

func mergeRateLimitSummary(current, next RateLimitExecutionSummary) RateLimitExecutionSummary {
	units := make(map[string]int64, len(current.Units)+len(next.Units))
	for i, unit := range current.Units {
		units[unit] += current.UnitTotals[i]
	}
	for i, unit := range next.Units {
		units[unit] += next.UnitTotals[i]
	}
	next.Units = sortedMapKeys(units)
	next.UnitTotals = make([]int64, len(next.Units))
	for i, unit := range next.Units {
		next.UnitTotals[i] = units[unit]
	}
	next.ScopeKinds = sortedUniqueStrings(append(current.ScopeKinds, next.ScopeKinds...))
	if next.PolicyCount < current.PolicyCount {
		next.PolicyCount = current.PolicyCount
	}
	next.ObservedDenials += current.ObservedDenials
	next.HeaderOutcome = mergeRateLimitHeaderOutcome(current.HeaderOutcome, next.HeaderOutcome)
	return next
}

func sortedMapKeys(values map[string]int64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedUniqueStrings(values []string) []string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}
	keys := make([]string, 0, len(unique))
	for value := range unique {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return keys
}

func mergeRateLimitHeaderOutcome(current, next string) string {
	priority := map[string]int{"": 0, "none": 1, "applied": 2, "error": 3, "invalid": 4}
	if priority[next] > priority[current] {
		return next
	}
	return current
}

func (t *ExecutionTimings) RateLimitSummary() RateLimitExecutionSummary {
	if t == nil {
		return RateLimitExecutionSummary{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	summary := t.rateLimit
	summary.ScopeKinds = append([]string(nil), summary.ScopeKinds...)
	summary.Units = append([]string(nil), summary.Units...)
	summary.UnitTotals = append([]int64(nil), summary.UnitTotals...)
	return summary
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
		attributeName := fmt.Sprintf("engine.timing.%s_ms", key)
		if strings.HasSuffix(key, "_ms") {
			attributeName = "engine.timing." + key
		}
		attrs = append(attrs, attribute.Float64(attributeName, snapshot[key]))
	}
	return attrs
}
