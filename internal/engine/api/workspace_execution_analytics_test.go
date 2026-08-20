package api

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
)

// TestWorkspaceExecutionRangeAcceptsBoundedRFC3339Range verifies exact browser
// timestamps survive validation without preset-specific server state.
func TestWorkspaceExecutionRangeAcceptsBoundedRFC3339Range(t *testing.T) {
	start := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	gotStart, gotEnd, err := workspaceExecutionRange(graphql.ResolveParams{Args: map[string]interface{}{
		"start_date": start.Format(time.RFC3339), "end_date": end.Format(time.RFC3339),
	}})
	if err != nil || !gotStart.Equal(start) || !gotEnd.Equal(end) {
		t.Fatalf("workspace range = %s..%s, error %v", gotStart, gotEnd, err)
	}
}

// TestWorkspaceExecutionRangeRejectsUnsafeRanges keeps malformed, reversed,
// and unbounded reporting scans outside the PostgreSQL analytics path.
func TestWorkspaceExecutionRangeRejectsUnsafeRanges(t *testing.T) {
	start := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{name: "malformed", args: map[string]interface{}{"start_date": "not-a-date"}, want: "invalid start_date"},
		{name: "reversed", args: map[string]interface{}{"start_date": start.Add(time.Hour).Format(time.RFC3339), "end_date": start.Format(time.RFC3339)}, want: "start_date must be before end_date"},
		{name: "too wide", args: map[string]interface{}{"start_date": start.Format(time.RFC3339), "end_date": start.Add(91 * 24 * time.Hour).Format(time.RFC3339)}, want: "activity range cannot exceed 90 days"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := workspaceExecutionRange(graphql.ResolveParams{Args: test.args})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestWorkspaceExecutionAnalyticsTelemetryUsesBoundedDuration verifies the
// read span remains useful without recording caller-selected timestamps.
func TestWorkspaceExecutionAnalyticsTelemetryUsesBoundedDuration(t *testing.T) {
	exporter := setupTestTracer(t)
	store := &workspaceTestStore{accountID: uuid.New()}
	handler := mountMCPGraphQLTestHandler(t, store)
	start := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	doMCPGraphQLRequestWithVariables(t, handler, `query Activity($start: String!, $end: String!) {
		workspaceExecutionAnalytics(start_date: $start, end_date: $end) { total_calls }
	}`, map[string]any{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)})
	attributes := map[string]string{}
	for _, span := range exporter.GetSpans() {
		if span.Name != "engine.graphql.workspace_execution_analytics.get" {
			continue
		}
		for _, item := range span.Attributes {
			attributes[string(item.Key)] = item.Value.Emit()
		}
	}
	if attributes["range.duration_hours"] != "24" {
		t.Fatalf("workspace analytics telemetry = %#v", attributes)
	}
	if _, exists := attributes["range.start"]; exists {
		t.Fatalf("workspace analytics telemetry exposed range.start: %#v", attributes)
	}
	if _, exists := attributes["range.end"]; exists {
		t.Fatalf("workspace analytics telemetry exposed range.end: %#v", attributes)
	}
}
