package retrypolicy

import (
	"testing"
	"time"
)

func TestBackoffDuration_ZeroBackoffIsAlwaysZero(t *testing.T) {
	if d := BackoffDuration("exponential", 0, 5); d != 0 {
		t.Errorf("expected 0 delay for backoffMs<=0, got %v", d)
	}
	if d := BackoffDuration("fixed", -10, 0); d != 0 {
		t.Errorf("expected 0 delay for negative backoffMs, got %v", d)
	}
}

func TestBackoffDuration_Fixed(t *testing.T) {
	for attempt := 0; attempt < 4; attempt++ {
		if d := BackoffDuration("fixed", 200, attempt); d != 200*time.Millisecond {
			t.Errorf("attempt %d: got %v, want 200ms flat", attempt, d)
		}
	}
	// Unrecognized/empty strategy behaves like "fixed" -- backward compatible
	// with RetryConfig rows that predate the exponential strategy.
	if d := BackoffDuration("", 150, 3); d != 150*time.Millisecond {
		t.Errorf("empty strategy: got %v, want 150ms flat", d)
	}
}

func TestBackoffDuration_Exponential(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{5, 3200 * time.Millisecond},  // 100 * 32 (ceiling)
		{20, 3200 * time.Millisecond}, // large attempt still capped at 32x
	}
	for _, c := range cases {
		if got := BackoffDuration("exponential", 100, c.attempt); got != c.want {
			t.Errorf("attempt %d: got %v, want %v", c.attempt, got, c.want)
		}
	}
}
