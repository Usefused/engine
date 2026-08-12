package retrypolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalFixtureValidates(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contract-fixtures", "retry", "v3_idempotency_predicates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
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
