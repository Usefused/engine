package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestOperatorYAMLAndEnvironment verifies partial YAML retains defaults and process settings win.
func TestOperatorYAMLAndEnvironment(t *testing.T) {
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "")
	t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", "")
	t.Setenv("FUSED_ENGINE_HTTP_PORT", "")
	body := "database:\n  max_conns: 7\n  max_conn_idle_time: 2m\nserver:\n  http_port: 9010\nnats:\n  url: nats://broker:4222\nobservability:\n  environment: staging\n  service_name: test-engine\n"
	cfg, err := loadAdmissionYAML(t, body)
	// Omitted neighboring fields must retain historical defaults.
	if err != nil {
		t.Fatal(err)
	}
	// Pool durations and numeric listener scalars must survive YAML decoding.
	if cfg.Database.MaxConns != 7 || cfg.Database.MaxConnIdleTime != 2*time.Minute || cfg.Server.HTTPPort != "9010" || cfg.Server.GRPCPort != "50051" || cfg.NATS.RateLimitReplicas != 1 {
		t.Fatal("YAML settings or defaults were lost")
	}
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "3")
	t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", "40s")
	t.Setenv("FUSED_ENGINE_HTTP_PORT", "9020")
	cfg, err = loadAdmissionYAML(t, body)
	// Only explicitly overridden fields may change.
	if err != nil {
		t.Fatal(err)
	}
	// Existing environment-only deployments retain priority over mounted YAML.
	if cfg.Database.MaxConns != 3 || cfg.Database.MaxConnIdleTime != 40*time.Second || cfg.Server.HTTPPort != "9020" || cfg.Observability.Environment != "staging" {
		t.Fatal("environment precedence or YAML preservation failed")
	}
}

// TestYAMLEnvironmentReferences verifies typed substitution and rejects structural injection through scalar data.
func TestYAMLEnvironmentReferences(t *testing.T) {
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "")
	t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", "")
	t.Setenv("FUSED_TEST_POOL", "6")
	t.Setenv("FUSED_TEST_IDLE", "45s")
	t.Setenv("FUSED_TEST_BROKER", "true")
	payload := "secret\"\nserver:\n  http_port: 9999\n${DO_NOT_EXPAND} # value"
	t.Setenv("FUSED_TEST_VALUE", payload)
	cfg, err := loadAdmissionYAML(t, "database:\n  max_conns: '${FUSED_TEST_POOL}'\n  max_conn_idle_time: ${FUSED_TEST_IDLE}\nengine:\n  managed_auth_broker_enabled: '${FUSED_TEST_BROKER}'\nobservability:\n  service_name: '${FUSED_TEST_VALUE}'\n  environment: 'prefix-${FUSED_TEST_POOL}'\n")
	// Typed references must decode through the same configuration schema as literal YAML.
	if err != nil {
		t.Fatal(err)
	}
	// Substitution may change a scalar value but never create sibling configuration keys or recurse into secrets.
	if cfg.Database.MaxConns != 6 || cfg.Database.MaxConnIdleTime != 45*time.Second || !cfg.Engine.ManagedAuthBrokerEnabled || cfg.Observability.ServiceName != payload || cfg.Observability.Environment != "prefix-6" || cfg.Server.HTTPPort != "8081" {
		t.Fatal("environment substitution changed structure or lost types")
	}
}

// TestYAMLMissingReferenceFails distinguishes missing variables from deliberately empty values.
func TestYAMLMissingReferenceFails(t *testing.T) {
	t.Setenv("FUSED_TEST_MISSING_REFERENCE", "")
	// Remove the variable while retaining test cleanup of its original environment state.
	if err := os.Unsetenv("FUSED_TEST_MISSING_REFERENCE"); err != nil {
		t.Fatal(err)
	}
	_, err := loadAdmissionYAML(t, "database:\n  url: '${FUSED_TEST_MISSING_REFERENCE}'\n")
	// A missing referenced variable must not silently become an empty credential or address.
	if err == nil || !strings.Contains(err.Error(), "FUSED_TEST_MISSING_REFERENCE") {
		t.Fatal("missing environment reference was not rejected")
	}
	t.Setenv("FUSED_TEST_MISSING_REFERENCE", "")
	_, err = loadAdmissionYAML(t, "observability:\n  service_name: '${FUSED_TEST_MISSING_REFERENCE}'\n")
	// Explicitly empty optional strings remain valid configuration.
	if err != nil {
		t.Fatal(err)
	}
}

// TestOperatorInvalidPoolSettings ensures YAML cannot bypass the same limits enforced on environment values.
func TestOperatorInvalidPoolSettings(t *testing.T) {
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "")
	t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", "")
	for _, field := range []string{"max_conns: 0", "max_conns: -1", "max_conn_idle_time: 0s", "max_conn_idle_time: later"} {
		_, err := loadAdmissionYAML(t, "database:\n  "+field+"\n")
		// Invalid operator input must fail before dependency initialization.
		if err == nil {
			t.Fatalf("accepted invalid setting %s", field)
		}
	}
}
