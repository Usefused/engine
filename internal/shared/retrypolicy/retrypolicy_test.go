package retrypolicy

import (
	"encoding/json"
	"testing"
)

const canonicalRetryV3 = `{"version":3,"rules":[{"predicates":{"methods":["GET"],"operation_kinds":["read"],"statuses":[{"min":429,"max":429}],"errors":[],"body_replayability":"any","idempotency_key":{"requirement":"any"},"required_provider_headers":[]},"action":{"max_attempts":3,"max_elapsed_ms":30000,"backoff":{"strategy":"exponential","base_delay_ms":250,"max_delay_ms":5000,"jitter_ms":100},"retry_after_headers":[]}}]}`

// TestCanonicalRetryV3Validates proves the standalone Engine accepts its exact
// retry v3 boundary without loading data from a sibling repository.
func TestCanonicalRetryV3Validates(t *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(canonicalRetryV3), &config); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&config); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLegacyRetryConfigFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`{"strategy":"fixed","max_retries":2,"backoff_ms":100}`,
		`{"version":3,"rules":[],"unknown":true}`,
		`{"version":3,"rules":[]} {}`,
	} {
		var config Config
		if err := json.Unmarshal([]byte(raw), &config); err == nil {
			t.Fatalf("non-canonical retry contract unexpectedly accepted: %s", raw)
		}
	}
}
