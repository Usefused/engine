package paginationpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestContractFixtures(t *testing.T) {
	valid := []string{
		"v3_token.json",
		"v3_offset.json",
		"v3_hybrid.json",
		"v3_rfc_link.json",
		"v3_graphql.json",
		"v3_graphql_templates.json",
		"v3_conditional_items.json",
		"v3_bare_array_cursor.json",
	}
	for _, name := range valid {
		t.Run(name, func(t *testing.T) {
			config := readContractFixture(t, name)
			if err := Validate(&config); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestLegacyContractFixturesFailClosed(t *testing.T) {
	for _, name := range []string{
		"v2_cursor_body.json",
		"v2_cursor_header_numeric.json",
		"v2_offset.json",
		"v2_offset_root_array.json",
		"v2_page_number.json",
		"v2_next_url_link.json",
	} {
		t.Run(name, func(t *testing.T) {
			assertContractDecodeFails(t, name)
		})
	}
}

func TestRootPathIsLimitedToItems(t *testing.T) {
	if err := ValidateItemsPath("$"); err != nil {
		t.Fatalf("ValidateItemsPath: %v", err)
	}
	if err := ValidateBodyPath("$"); err == nil {
		t.Fatal("root continuation source unexpectedly accepted")
	}
}

func TestInvalidContractFixturesFailClosed(t *testing.T) {
	for _, name := range []string{"invalid_legacy_shape.json", "invalid_multiple_strategies.json"} {
		t.Run(name, func(t *testing.T) {
			assertContractDecodeFails(t, name)
		})
	}
}

func TestStrictDecodeRejectsUnknownNestedFieldAndTrailingData(t *testing.T) {
	for _, raw := range []string{
		`{"version":3,"request":[],"response":{"items":{"path":"$","unknown":true},"values":[]},"continuation":[],"termination":{"repeated_value":"stop"},"limits":{}}`,
		`{"version":3,"request":[],"response":{"items":{"path":"$"},"values":[]},"continuation":[],"termination":{"repeated_value":"stop"},"limits":{}} {}`,
	} {
		var config Config
		if err := json.Unmarshal([]byte(raw), &config); err == nil {
			t.Fatalf("non-canonical pagination contract unexpectedly accepted: %s", raw)
		}
	}
}

func TestEffectiveLimitsAppliesFrozenDefaults(t *testing.T) {
	got := EffectiveLimits(Limits{})
	if got.MaxPages != 100 || got.MaxItems != 10_000 || got.MaxBytes != 16_777_216 || got.MaxDurationMs != 120_000 {
		t.Fatalf("unexpected defaults: %+v", got)
	}
}

func readContractFixture(t *testing.T, name string) Config {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "contract-fixtures", "pagination", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return config
}

func assertContractDecodeFails(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "contract-fixtures", "pagination", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err == nil {
		t.Fatal("non-canonical pagination contract unexpectedly accepted")
	}
}
