package sandbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/models"
)

func TestAttachExecutionTimingsIncludesBoundedPaginationSummary(t *testing.T) {
	timings := engine.NewExecutionTimings()
	ctx := engine.ContextWithExecutionTimings(context.Background(), timings)
	engine.RecordPaginationSummary(ctx, engine.PaginationExecutionSummary{
		Type: "cursor", PageCount: 2, ItemCount: 3, ByteCount: 128, StopReason: "missing_next",
	})
	event := models.EngineExecutionEvent{}

	attachExecutionTimings(ctx, &event)

	if event.PaginationType != "cursor" || event.PaginationPageCount != 2 || event.PaginationItemCount != 3 || event.PaginationByteCount != 128 || event.PaginationStopReason != "missing_next" {
		t.Fatalf("pagination activity summary = %+v", event)
	}
}

func TestClassifyExecutionFailureUsesTypedPaginationCode(t *testing.T) {
	category, code := classifyExecutionFailure(&engine.PaginationError{Code: "cycle"}, 0)
	if category != "pagination" || code != "cycle" {
		t.Fatalf("classification = %q/%q", category, code)
	}
}

func TestAttachExecutionTimingsIncludesSafeRateLimitAggregate(t *testing.T) {
	timings := engine.NewExecutionTimings()
	ctx := engine.ContextWithExecutionTimings(context.Background(), timings)
	engine.RecordRateLimitSummary(ctx, engine.RateLimitExecutionSummary{
		Decision: "allowed", PolicyCount: 2, ScopeKinds: []string{"connection", "service_version"},
		Units: []string{"points", "requests"}, UnitTotals: []int64{10, 1}, RetryOutcome: "waited",
	})
	engine.RecordRateLimitHeaderOutcome(ctx, "applied")
	engine.AddExecutionTiming(ctx, "rate_limit_acquire_ms", 3*time.Millisecond)
	event := models.EngineExecutionEvent{}

	attachExecutionTimings(ctx, &event)

	if event.RateLimitDecision != "allowed" || event.RateLimitPolicyCount != 2 || event.RateLimitRetryOutcome != "waited" || event.RateLimitHeaderOutcome != "applied" {
		t.Fatalf("rate-limit activity summary = %+v", event)
	}
	if len(event.RateLimitUnits) != 2 || event.RateLimitUnits[0] != "points" || event.RateLimitUnitTotals[0] != 10 {
		t.Fatalf("rate-limit unit totals = %#v/%#v", event.RateLimitUnits, event.RateLimitUnitTotals)
	}
	var timingSnapshot map[string]float64
	if err := json.Unmarshal(event.Timings, &timingSnapshot); err != nil {
		t.Fatal(err)
	}
	if timingSnapshot["rate_limit_acquire_ms"] != 3 {
		t.Fatalf("rate-limit acquisition timing = %#v", timingSnapshot)
	}
}
