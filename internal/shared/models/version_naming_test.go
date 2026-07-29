package models

import (
	"testing"
	"time"
)

// TestComputeNextVersionName is a pure, DB-free unit test for the
// server-generated version naming scheme: date-based (YYYY-MM-DD), with a
// -2/-3/... suffix on same-day collisions. No DB required --
// ComputeNextVersionName only needs the set of names that already exist for
// the service, not how to fetch them. Moved here (from
// repository.computeNextVersionName) so the OpenAPI import path
// (openapi.ParseSpec's no-explicit-version fallback) can reuse the exact same
// function instead of growing its own date-naming logic.
func TestComputeNextVersionName(t *testing.T) {
	fixedNow := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		existing map[string]bool
		want     string
	}{
		{"no versions yet", map[string]bool{}, "2026-07-08"},
		{"unrelated older date exists", map[string]bool{"2026-07-01": true}, "2026-07-08"},
		{"today already published once", map[string]bool{"2026-07-08": true}, "2026-07-08-2"},
		{"today published twice", map[string]bool{"2026-07-08": true, "2026-07-08-2": true}, "2026-07-08-3"},
		{"gap in suffixes still finds first free slot", map[string]bool{"2026-07-08": true, "2026-07-08-3": true}, "2026-07-08-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeNextVersionName(tt.existing, fixedNow)
			if got != tt.want {
				t.Errorf("ComputeNextVersionName(%v) = %q, want %q", tt.existing, got, tt.want)
			}
		})
	}
}
