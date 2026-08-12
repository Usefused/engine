package ratelimitpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFrozenFixtures explicitly validates the complete policy inventory so no
// fixture relies solely on a broad CLI filename glob for coverage.
func TestFrozenFixtures(t *testing.T) {
	for _, name := range []string{"v3_shared_credential.json", "v3_method_concurrency.json", "v3_simultaneous_windows.json", "v3_weighted_burst_tenant.json", "v3_dynamic_headers.json", "v3_quota_units.json", "v3_complexity.json", "v3_composite_identity.json", "v3_concurrency.json", "v3_account_operation_concurrency.json", "v3_hourly_connection.json"} {
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
	policy := Policy{Cost: CostPlan{Default: 1, Rules: []CostRule{{Operation: "rest:GET:/drive/v3/files/{}", Cost: 10}}}}
	if got := policy.ResolvedCost("rest:GET:/drive/v3/files/{}"); got != 10 {
		t.Fatalf("matching stable key cost = %d, want 10", got)
	}
	if got := policy.ResolvedCost("getFile"); got != 1 {
		t.Fatalf("unmatched operation name cost = %d, want default 1", got)
	}
}

func TestStrictValidationBounds(t *testing.T) {
	valid := fixedWindowConfig()
	tests := map[string]func(*Config){
		"duplicate names":       func(c *Config) { c.Policies = append(c.Policies, c.Policies[0]) },
		"invalid name":          func(c *Config) { c.Policies[0].Name = "bad name" },
		"invalid unit":          func(c *Config) { c.Policies[0].Unit = "widgets" },
		"negative default cost": func(c *Config) { c.Policies[0].Cost.Default = -1 },
		"no positive cost":      func(c *Config) { c.Policies[0].Cost.Default = 0 },
		"oversized limit":       func(c *Config) { c.Policies[0].FixedWindow.Limit = maxPolicyValue + 1 },
		"oversized interval":    func(c *Config) { c.Policies[0].FixedWindow.DurationMs = maxIntervalMS + 1 },
		"empty headers":         func(c *Config) { c.Policies[0].ResponseSignals = &ResponseSignals{} },
		"invalid reset format": func(c *Config) {
			c.Policies[0].ResponseSignals = &ResponseSignals{Reset: &ResetSignal{Signal: ResponseSignal{Source: ResponseSignalHeader, Name: "X-Reset"}, Format: "date"}}
		},
		"untrimmed operation key": func(c *Config) { c.Policies[0].Cost.Rules = []CostRule{{Operation: " key", Cost: 1}} },
		"newline operation key":   func(c *Config) { c.Policies[0].Cost.Rules = []CostRule{{Operation: "key\nnext", Cost: 1}} },
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
	if config.Version != 0 || config.Policies != nil || config.Cooldown != nil {
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
	config.Policies[0].Cost.Rules = make([]CostRule, maxCostRules+1)
	for i := 0; i <= maxCostRules; i++ {
		config.Policies[0].Cost.Rules[i] = CostRule{Operation: strings.Repeat("x", i%500+1) + string(rune(i)), Cost: 1}
	}
	if err := config.Validate(); err == nil {
		t.Fatal("expected operation cost count overflow")
	}
}

func fixedWindowConfig() Config {
	return Config{Version: Version, Policies: []Policy{{
		Name: "requests", Mode: ModeEnforce, Unit: UnitRequests,
		Identity: BucketIdentity{Inputs: []IdentityInput{{Kind: IdentityServiceVersion}}}, Cost: CostPlan{Default: 1}, Algorithm: AlgorithmFixedWindow,
		FixedWindow: &FixedWindow{Limit: 100, DurationMs: 60_000},
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
