package main

import (
	"strings"
	"testing"
)

func TestParseBenchmarkOutputRequiresAndGroupsEveryPhase(t *testing.T) {
	lines := []string{
		"BenchmarkAuthorizationAcceptance/cold_auth/memberships_1_bindings_100-10 20 1000 ns/op",
		"BenchmarkAuthorizationAcceptance/cache_hit/memberships_1_bindings_100-10 20 100 ns/op",
		"BenchmarkAuthorizationAcceptance/authorization_check_all/memberships_1_bindings_100-10 20 500 ns/op",
		"BenchmarkAuthorizationAcceptance/graphql_preflight/fields_1-10 20 2000 ns/op 3 db_queries/op 1 external_queries/op",
		"BenchmarkAuthorizationAcceptance/total_request/fields_1-10 20 3000 ns/op",
	}
	groups, err := parseBenchmarkOutput([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("parse benchmark output: %v", err)
	}
	preflight := groups[benchmarkName+"/graphql_preflight/fields_1"]
	if len(preflight) != 1 || preflight[0].databaseCalls == nil || *preflight[0].databaseCalls != 3 {
		t.Fatalf("preflight samples = %#v", preflight)
	}
}

func TestParseBenchmarkOutputRejectsMissingPhase(t *testing.T) {
	_, err := parseBenchmarkOutput([]byte(
		"BenchmarkAuthorizationAcceptance/cache_hit/memberships_1_bindings_100-10 20 100 ns/op\n",
	))
	if err == nil || !strings.Contains(err.Error(), "cold_auth") {
		t.Fatalf("missing phase error = %v", err)
	}
}

func TestPercentileUsesNearestRank(t *testing.T) {
	values := make([]float64, 20)
	for index := range values {
		values[index] = float64(index + 1)
	}
	if got := percentile(values, 0.50); got != 10 {
		t.Fatalf("p50 = %v, want 10", got)
	}
	if got := percentile(values, 0.95); got != 19 {
		t.Fatalf("p95 = %v, want 19", got)
	}
	if got := percentile(values, 0.99); got != 20 {
		t.Fatalf("p99 = %v, want 20", got)
	}
}
