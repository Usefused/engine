package ratelimitpolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCanonicalAlgorithmsValidate validates every Engine algorithm from local typed cases;
// cross-runtime fixture inventory coverage belongs to the integration suite.
func TestCanonicalAlgorithmsValidate(t *testing.T) {
	for name, config := range canonicalRateLimitCases() {
		t.Run(name, func(t *testing.T) {
			if err := config.Validate(); err != nil {
				t.Fatalf("validate %s: %v", name, err)
			}
		})
	}
}

// TestInvalidContractsFailClosed retains representative obsolete and
// discriminator-conflict inputs as inline negative evidence only.
func TestInvalidContractsFailClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy":        `{"version":2,"scope":"connection","default_cost":1}`,
		"discriminator": `{"version":3,"policies":[{"name":"requests","mode":"enforce","unit":"requests","identity":{"inputs":[{"kind":"service_version"}]},"cost":{"default":1,"rules":[]},"algorithm":"fixed_window","fixed_window":{"limit":100,"duration_ms":60000},"concurrency":{"limit":2}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var config Config
			if err := json.Unmarshal([]byte(raw), &config); err == nil {
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

// fixedWindowConfig is the canonical base used by mutation and algorithm tests
// so validation differences come only from the behavior under test.
func fixedWindowConfig() Config {
	return Config{Version: Version, Policies: []Policy{{
		Name: "requests", Mode: ModeEnforce, Unit: UnitRequests,
		Identity: BucketIdentity{Inputs: []IdentityInput{{Kind: IdentityServiceVersion}}}, Cost: CostPlan{Default: 1}, Algorithm: AlgorithmFixedWindow,
		FixedWindow: &FixedWindow{Limit: 100, DurationMs: 60_000},
	}}}
}

// canonicalRateLimitCases derives every algorithm from one valid base so
// identity and cost semantics cannot drift between standalone Engine tests.
func canonicalRateLimitCases() map[string]Config {
	rolling := fixedWindowConfig()
	rolling.Policies[0].Algorithm = AlgorithmRollingWindow
	rolling.Policies[0].FixedWindow = nil
	rolling.Policies[0].RollingWindow = &RollingWindow{Limit: 100, DurationMs: 60_000}
	token := fixedWindowConfig()
	token.Policies[0].Algorithm = AlgorithmTokenBucket
	token.Policies[0].FixedWindow = nil
	token.Policies[0].TokenBucket = &TokenBucket{Capacity: 100, RefillUnits: 10, RefillIntervalMs: 1_000}
	concurrency := fixedWindowConfig()
	concurrency.Policies[0].Algorithm = AlgorithmConcurrency
	concurrency.Policies[0].FixedWindow = nil
	concurrency.Policies[0].Concurrency = &Concurrency{Limit: 4}
	return map[string]Config{
		"fixed_window": fixedWindowConfig(), "rolling_window": rolling,
		"token_bucket": token, "concurrency": concurrency,
	}
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
