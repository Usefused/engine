package ratelimitpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrozenFixtures(t *testing.T) {
	for _, name := range []string{"v2_fixed_window.json", "v2_token_bucket.json", "v2_mixed.json"} {
		t.Run(name, func(t *testing.T) {
			var config Config
			if err := json.Unmarshal(readFixture(t, name), &config); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if config.Version != Version {
				t.Fatalf("version = %d, want %d", config.Version, Version)
			}
		})
	}
}

func TestFrozenInvalidFixturesFailClosed(t *testing.T) {
	for _, name := range []string{"invalid_legacy.json", "invalid_discriminator.json"} {
		t.Run(name, func(t *testing.T) {
			var config Config
			if err := json.Unmarshal(readFixture(t, name), &config); err == nil {
				t.Fatal("expected invalid rate-limit contract")
			}
		})
	}
}

func TestOperationCostUsesOnlyTransportedStableKey(t *testing.T) {
	policy := Policy{DefaultCost: 1, OperationCosts: map[string]int64{"rest:GET:/drive/v3/files/{}": 10}}
	if got := policy.Cost("rest:GET:/drive/v3/files/{}"); got != 10 {
		t.Fatalf("matching stable key cost = %d, want 10", got)
	}
	if got := policy.Cost("getFile"); got != 1 {
		t.Fatalf("unmatched operation name cost = %d, want default 1", got)
	}
}

func TestStrictValidationBounds(t *testing.T) {
	valid := fixedWindowConfig()
	tests := map[string]func(*Config){
		"duplicate names":       func(c *Config) { c.Policies = append(c.Policies, c.Policies[0]) },
		"invalid name":          func(c *Config) { c.Policies[0].Name = "bad name" },
		"invalid unit":          func(c *Config) { c.Policies[0].Unit = "widgets" },
		"negative default cost": func(c *Config) { c.Policies[0].DefaultCost = -1 },
		"no positive cost":      func(c *Config) { c.Policies[0].DefaultCost = 0 },
		"oversized limit":       func(c *Config) { c.Policies[0].FixedWindow.Limit = maxPolicyValue + 1 },
		"oversized interval":    func(c *Config) { c.Policies[0].FixedWindow.DurationMS = maxIntervalMS + 1 },
		"empty headers":         func(c *Config) { c.Policies[0].ResponseHeaders = &ResponseHeaders{} },
		"invalid reset format": func(c *Config) {
			c.Policies[0].ResponseHeaders = &ResponseHeaders{Reset: &ResetHeader{Name: "X-Reset", Format: "date"}}
		},
		"disabled retry pointer":  func(c *Config) { c.RetryAfter = &RetryAfter{MaxDelayMS: 1} },
		"untrimmed operation key": func(c *Config) { c.Policies[0].OperationCosts[" key"] = 1 },
		"newline operation key":   func(c *Config) { c.Policies[0].OperationCosts["key\nnext"] = 1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := cloneConfig(t, valid)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestScanNilClearsReusedReceiver(t *testing.T) {
	config := fixedWindowConfig()
	if err := config.Scan(nil); err != nil {
		t.Fatal(err)
	}
	if config.Version != 0 || config.Policies != nil || config.RetryAfter != nil {
		t.Fatalf("nil scan retained stale state: %#v", config)
	}
}

func TestStrictDecodeRejectsUnknownAndTrailingData(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"policies":[],"unexpected":true}`,
		`{"version":2,"policies":[]} {}`,
		`{"version":2,"policies":[{"name":"burst","unit":"requests","scope":"connection","default_cost":1,"operation_costs":{},"algorithm":"token_bucket","token_bucket":{"capacity":10,"refill_units":1,"refill_interval_ms":1000,"burst":5}}]}`,
	} {
		var config Config
		if err := json.Unmarshal([]byte(raw), &config); err == nil {
			t.Fatalf("expected strict decode failure for %s", raw)
		}
	}
}

func TestMaximumPolicyAndOperationCounts(t *testing.T) {
	config := fixedWindowConfig()
	config.Policies = make([]Policy, MaxPolicies)
	for i := range config.Policies {
		config.Policies[i] = fixedWindowConfig().Policies[0]
		config.Policies[i].Name = "policy_" + strings.Repeat("x", i)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("maximum policy count: %v", err)
	}
	config.Policies = append(config.Policies, fixedWindowConfig().Policies[0])
	if err := config.Validate(); err == nil {
		t.Fatal("expected policy count overflow")
	}

	config = fixedWindowConfig()
	config.Policies[0].OperationCosts = make(map[string]int64, maxOperationCosts+1)
	for i := 0; i <= maxOperationCosts; i++ {
		config.Policies[0].OperationCosts[strings.Repeat("x", i%500+1)+string(rune(i))] = 1
	}
	if err := config.Validate(); err == nil {
		t.Fatal("expected operation cost count overflow")
	}
}

func fixedWindowConfig() Config {
	return Config{Version: Version, Policies: []Policy{{
		Name: "requests", Unit: "requests", Scope: "service_version", DefaultCost: 1,
		OperationCosts: map[string]int64{}, Algorithm: "fixed_window",
		FixedWindow: &FixedWindow{Limit: 100, DurationMS: 60_000},
	}}}
}

func cloneConfig(t *testing.T, config Config) Config {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var clone Config
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "contract-fixtures", "rate-limit", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}
