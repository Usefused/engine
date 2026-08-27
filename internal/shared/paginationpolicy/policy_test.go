package paginationpolicy

import (
	"encoding/json"
	"testing"
)

// TestCanonicalContractsValidate validates representative Engine-owned state graphs;
// cross-runtime corpus coverage remains in the integration workspace.
func TestCanonicalContractsValidate(t *testing.T) {
	for name, config := range canonicalPaginationCases() {
		t.Run(name, func(t *testing.T) {
			if err := Validate(&config); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestLegacyContractsFailClosed keeps representative removed shapes as
// negative evidence without making Engine depend on a legacy fixture tree.
func TestLegacyContractsFailClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"cursor":      `{"version":2,"type":"cursor","cursor":{"request_parameter":"cursor"}}`,
		"offset":      `{"version":2,"type":"offset","offset":{"request_parameter":"offset"}}`,
		"page_number": `{"version":2,"type":"page_number","page_number":{"request_parameter":"page"}}`,
		"next_url":    `{"version":2,"type":"next_url","next_url":{"path":"$.next"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertContractDecodeFails(t, raw)
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

// TestInvalidContractsFailClosed covers invalid canonical composition
// locally; the shared golden inventory is tested outside standalone Engine CI.
func TestInvalidContractsFailClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy_shape":    `{"type":"cursor","cursor":{"request_parameter":"cursor"}}`,
		"duplicate_state": `{"version":3,"request":[],"response":{"items":{"path":"$"},"values":[]},"continuation":[{"kind":"next_url","state":"next","response_value":"missing","origin":{"mode":"same_origin"}},{"kind":"next_url","state":"next","response_value":"missing","origin":{"mode":"same_origin"}}],"termination":{"stop_on_empty_items":true,"repeated_value":"stop"},"limits":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertContractDecodeFails(t, raw)
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

// TestMissingItemsEmptyOptInSurvivesStrictDecode proves the reviewed response
// invariant crosses Engine's strict Registry boundary without weakening unknown-field rejection.
func TestMissingItemsEmptyOptInSurvivesStrictDecode(t *testing.T) {
	raw := `{"version":3,"request":[{"state":"cursor","target":{"location":"query","name":"cursor"},"value_type":"string","apply":"subsequent"}],"response":{"items":{"path":"$.items","missing_is_empty":true},"values":[{"name":"next","source":{"location":"body","path":"$.next","value_type":"string"}}]},"continuation":[{"kind":"token","state":"cursor","response_value":"next"}],"termination":{"stop_on_missing_values":["next"],"repeated_value":"stop"},"limits":{"max_pages":10,"max_items":100,"max_bytes":1024,"max_duration_ms":1000}}`
	var config Config
	// The strict decoder must recognize the reviewed field rather than classify it as unknown input.
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("decode reviewed omission policy: %v", err)
	}
	// Successful decoding must retain the executable behavior bit.
	if !config.Response.Items.MissingIsEmpty {
		t.Fatal("reviewed omission flag was lost")
	}
	config.Response.Items.Path = "$"
	// Root documents are always present, so applying omission semantics there is invalid configuration.
	if err := Validate(&config); err == nil {
		t.Fatal("root collection accepted missing_is_empty")
	}
}

func TestEffectiveLimitsAppliesFrozenDefaults(t *testing.T) {
	got := EffectiveLimits(Limits{})
	if got.MaxPages != 100 || got.MaxItems != 10_000 || got.MaxBytes != 16_777_216 || got.MaxDurationMs != 120_000 {
		t.Fatalf("unexpected defaults: %+v", got)
	}
}

// canonicalPaginationCases keeps each continuation family explicit while
// avoiding duplicated filesystem inventories inside the Engine repository.
func canonicalPaginationCases() map[string]Config {
	zero := int64(0)
	return map[string]Config{
		"token": {
			Version:      Version,
			Request:      []RequestStep{{State: "cursor", Target: RequestTarget{Location: RequestQuery, Name: "cursor"}, ValueType: ValueString, Apply: ApplyAll}},
			Response:     ResponsePlan{Items: ItemsSource{Path: "$.items"}, Values: []ResponseValue{{Name: "next", Source: ValueSource{Location: SourceBody, Path: "$.next", ValueType: ValueString}}}},
			Continuation: []ContinuationStep{{Kind: ContinuationToken, State: "cursor", ResponseValue: "next"}},
			Termination:  Termination{StopOnMissingValues: []string{"next"}, RepeatedValue: RepeatedStop},
		},
		"offset": {
			Version:      Version,
			Request:      []RequestStep{{State: "offset", Target: RequestTarget{Location: RequestQuery, Name: "offset"}, ValueType: ValueInteger, Initial: &Scalar{Type: ValueInteger, Integer: &zero}, Apply: ApplyAll}},
			Response:     ResponsePlan{Items: ItemsSource{Path: "$.items"}},
			Continuation: []ContinuationStep{{Kind: ContinuationOffset, State: "offset", Increment: &Increment{Mode: IncrementItemsReturned}}},
			Termination:  Termination{StopOnEmptyItems: true, RepeatedValue: RepeatedStop},
		},
		"next_url": {
			Version:      Version,
			Response:     ResponsePlan{Items: ItemsSource{Path: "$.items"}, Values: []ResponseValue{{Name: "next", Source: ValueSource{Location: SourceBody, Path: "$.next", ValueType: ValueURL}}}},
			Continuation: []ContinuationStep{{Kind: ContinuationNextURL, State: "next_url", ResponseValue: "next", Origin: &OriginPolicy{Mode: OriginSame}}},
			Termination:  Termination{StopOnMissingValues: []string{"next"}, RepeatedValue: RepeatedStop},
		},
	}
}

// assertContractDecodeFails proves strict JSON decoding rejects obsolete or
// internally inconsistent inputs before they reach pagination execution.
func assertContractDecodeFails(t *testing.T, raw string) {
	t.Helper()
	var config Config
	if err := json.Unmarshal([]byte(raw), &config); err == nil {
		t.Fatal("non-canonical pagination contract unexpectedly accepted")
	}
}
