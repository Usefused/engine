package config

import (
	"path/filepath"
	"testing"
)

// TestAgentDefaultAndOptOut checks real configuration loading so an omitted flag differs from an explicit opt-out.
func TestAgentDefaultAndOptOut(t *testing.T) {
	for _, test := range []struct {
		name, yaml, environment string
		missing, want           bool
	}{
		{name: "missing config", missing: true, want: true},
		{name: "empty config", want: true},
		{name: "unrelated engine setting", yaml: "engine:\n  execution_retention_days: 7\n", want: true},
		{name: "partial agent config", yaml: "engine:\n  ai:\n    cache_dir: /tmp/fused-agent-test\n", want: true},
		{name: "yaml opt out", yaml: "engine:\n  ai:\n    enabled: false\n", want: false},
		{name: "environment opt out", environment: "false", want: false},
		{name: "environment overrides yaml enable", yaml: "engine:\n  ai:\n    enabled: true\n", environment: "false", want: false},
		{name: "environment overrides yaml opt out", yaml: "engine:\n  ai:\n    enabled: false\n", environment: "true", want: true},
	} {
		// Keep each deployment scenario independent of inherited or previous-test environment settings.
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironmentExport(t)
			// An absent variable must exercise the default, not an explicitly invalid empty boolean.
			if test.environment != "" {
				t.Setenv("FUSED_AGENT_ENABLED", test.environment)
			}
			var cfg *Config
			var err error
			// Environment-only startup must follow the same default as partial YAML deployments.
			if test.missing {
				cfg, err = Load(filepath.Join(t.TempDir(), "missing.yaml"))
			} else {
				cfg, err = loadAdmissionYAML(t, test.yaml)
			}
			// A load failure cannot count as a successfully resolved enablement policy.
			if err != nil {
				t.Fatal(err)
			}
			// Explicit operator choices remain authoritative over the new default.
			if cfg.Engine.AI.Enabled != test.want {
				t.Fatalf("agent enabled = %v, want %v", cfg.Engine.AI.Enabled, test.want)
			}
		})
	}
}

// TestAgentEndpointsRejectCredentialRedirects confines plaintext model transport to local development.
func TestAgentEndpointsRejectCredentialRedirects(t *testing.T) {
	for _, endpoint := range []string{"http://remote.example/v1", "https://user:password@example.com", "https://example.com?token=x", "https://example.com?", "file:///tmp/key"} {
		// Unsafe endpoints must fail before optional enablement can transmit credentials.
		if ValidateAgentURL(endpoint) == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"https://registry.example/agent/v1", "http://localhost:9000/v1", "http://127.0.0.1:9000/v1"} {
		// TLS and explicit loopback are the supported operator choices.
		if err := ValidateAgentURL(endpoint); err != nil {
			t.Errorf("valid endpoint: %v", err)
		}
	}
}

// TestAgentEnvironmentOverridesYAML preserves installations configured through existing environment variables.
func TestAgentEnvironmentOverridesYAML(t *testing.T) {
	t.Setenv("FUSED_AGENT_ENABLED", "true")
	t.Setenv("FUSED_AGENT_MODEL", "environment-model")
	value, err := ResolveAgentConfig(AgentConfig{Gateway: AgentGatewayConfig{Model: "yaml-model"}})
	if err != nil || !value.Enabled || value.Gateway.Model != "environment-model" {
		t.Fatalf("environment override: %#v %v", value, err)
	} // Explicit environment settings remain authoritative.
	t.Setenv("FUSED_AGENT_ENABLED", "invalid")
	if _, err := ResolveAgentConfig(value); err == nil {
		t.Fatal("malformed enablement accepted")
	} // Typos cannot silently enable paid inference.
}
