package executionappvm

import (
	"context"
	"time"
)

// PhaseTiming contains only Engine-defined execution stages, never authored payloads or exceptions.
type PhaseTiming struct {
	Name      string        `json:"name"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration_ns"`
	Failed    bool          `json:"failed"`
}

// PhaseObserver receives bounded execution evidence independently of provider effects and replay ordinals.
type PhaseObserver interface {
	RecordExecutionPhases(context.Context, []PhaseTiming)
}

type phaseClock struct {
	entries []PhaseTiming
	started time.Time
}

// next closes the preceding stage using the real monotonic clock rather than authored deterministic time.
func (clock *phaseClock) next(name string) {
	now := time.Now()
	// Only the fixed sequence can advance; authored calls cannot create extra telemetry dimensions.
	if len(clock.entries) >= len(executionPhases) || name != executionPhases[len(clock.entries)] {
		return
	}
	if len(clock.entries) > 0 {
		clock.entries[len(clock.entries)-1].Duration = now.Sub(clock.started)
	}
	clock.started = now
	clock.entries = append(clock.entries, PhaseTiming{Name: name, StartedAt: now})
}

var executionPhases = []string{"compilation", "initialization", "input_validation", "execute", "output_validation"}

// finish preserves the last reached phase on both normal return and an authored exception.
func (clock *phaseClock) finish(failed bool) []PhaseTiming {
	// Requests rejected before execution have no invented phase measurement.
	if len(clock.entries) == 0 {
		return nil
	}
	last := &clock.entries[len(clock.entries)-1]
	last.Duration = time.Since(clock.started)
	last.Failed = failed
	return clock.entries
}

// ValidExecutionPhases bounds trusted IPC evidence again before it reaches telemetry or durable history.
func ValidExecutionPhases(phases []PhaseTiming) bool {
	// A run may fail before reaching later stages but cannot contain arbitrary or repeated stages.
	if len(phases) > len(executionPhases) {
		return false
	}
	now := time.Now()
	for i, phase := range phases {
		// Wall-clock jumps and malformed child frames cannot become unbounded durations or span timestamps.
		if phase.Name != executionPhases[i] || phase.StartedAt.IsZero() || phase.Duration < 0 || phase.Duration > 2*WallTime || phase.StartedAt.Before(now.Add(-2*WallTime)) || phase.StartedAt.After(now.Add(time.Second)) {
			return false
		}
	}
	return true
}
