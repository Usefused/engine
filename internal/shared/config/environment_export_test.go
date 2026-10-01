package config

import (
	"os"
	"testing"
)

// isolateEnvironmentExport makes startup publication reversible and independent of the developer's shell.
func isolateEnvironmentExport(t *testing.T) {
	t.Helper()
	for _, name := range yamlEnvironmentBindings {
		t.Setenv(name, "")
		// Admission distinguishes absent overrides from explicitly invalid empty values.
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"DATABASE_URL", "NATS_TOKEN", "NATS_USERNAME", "NATS_PASSWORD"} {
		t.Setenv(name, "")
	}
}

// TestYAMLExportsToExistingEnvironmentConsumers proves explicitly declared settings become ordinary process variables.
func TestYAMLExportsToExistingEnvironmentConsumers(t *testing.T) {
	isolateEnvironmentExport(t)
	cfg, err := loadAdmissionYAML(t, `database:
  url: postgres://localhost/fused
  max_conns: 7
  max_conn_idle_time: 2m
server:
  http_port: 9010
nats:
  url: nats://localhost:4222
  rate_limit_replicas: 3
  credentials_file: /run/secrets/nats.creds
  tls:
    server_name: nats.example
observability:
  environment: staging
  service_name: custom-engine
  otel_target: localhost:4318
  traces_endpoint: http://traces:4318/v1/traces
engine:
  unified_apps:
    max_concurrency: 2
`)
	// Config inspection itself must remain free from process mutation.
	if err != nil {
		t.Fatal(err)
	}
	// YAML values become environment defaults only at the explicit startup boundary.
	if os.Getenv("FUSED_DATABASE_MAX_CONNS") != "" {
		t.Fatal("Load unexpectedly mutated environment")
	}
	// Publication must succeed before any consumer initializes its own settings.
	if err := cfg.ExportEnvironment(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"FUSED_DATABASE_URL": "postgres://localhost/fused", "FUSED_DATABASE_MAX_CONNS": "7", "FUSED_DATABASE_MAX_CONN_IDLE_TIME": "2m",
		"FUSED_ENGINE_HTTP_PORT": "9010", "NATS_URL": "nats://localhost:4222", "FUSED_NATS_RATE_LIMIT_REPLICAS": "3",
		"NATS_CREDS_FILE": "/run/secrets/nats.creds", "NATS_TLS_SERVER_NAME": "nats.example", "FUSED_ENGINE_ENVIRONMENT": "staging",
		"OTEL_SERVICE_NAME": "custom-engine", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://traces:4318/v1/traces", "FUSED_UNIFIED_APP_MAX_CONCURRENCY": "2",
	} {
		// Existing os.Getenv consumers must observe the exact declared deployment setting.
		if got := os.Getenv(name); got != want {
			t.Fatalf("%s did not receive YAML setting", name)
		}
	}
	// Defaults for omitted fields must not be injected into the process environment.
	if os.Getenv("FUSED_ENGINE_GRPC_PORT") != "" || os.Getenv("FUSED_UNIFIED_APP_QUEUE_CAPACITY") != "" {
		t.Fatal("exported an undeclared YAML default")
	}
}

// TestInheritedEnvironmentWins verifies existing deployments and grouped aliases are preserved.
func TestInheritedEnvironmentWins(t *testing.T) {
	isolateEnvironmentExport(t)
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "4")
	t.Setenv("DATABASE_URL", "postgres://localhost/inherited")
	t.Setenv("NATS_TOKEN", "test-token")
	t.Setenv("NATS_URL", "nats://inherited:4222")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://inherited:4318")
	t.Setenv("FUSED_ENGINE_ENVIRONMENT", "production")
	t.Setenv("FUSED_ENGINE_GRPC_PORT", "7000")
	cfg, err := loadAdmissionYAML(t, `database:
  url: postgres://localhost/yaml
  max_conns: 7
nats:
  url: nats://yaml:4222
  credentials_file: /run/secrets/yaml.creds
observability:
  environment: staging
  traces_endpoint: http://yaml:4318/v1/traces
`)
	// YAML remains valid even when an existing environment source owns the effective value.
	if err != nil {
		t.Fatal(err)
	}
	// Startup must not overwrite the inherited sources or their aliases.
	if err := cfg.ExportEnvironment(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"FUSED_DATABASE_MAX_CONNS": "4", "DATABASE_URL": "postgres://localhost/inherited", "FUSED_DATABASE_URL": "",
		"NATS_URL": "nats://inherited:4222", "NATS_TOKEN": "test-token", "NATS_CREDS_FILE": "",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://inherited:4318", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "",
		"FUSED_ENGINE_ENVIRONMENT": "production", "FUSED_ENGINE_GRPC_PORT": "7000",
	} {
		// Unspecified YAML leaves environment-only configuration available to every existing consumer.
		if got := os.Getenv(name); got != want {
			t.Fatalf("%s did not preserve environment precedence", name)
		}
	}
}

// TestEnvironmentExportSupportsYAMLMerge ensures normal YAML anchors do not silently lose exported settings.
func TestEnvironmentExportSupportsYAMLMerge(t *testing.T) {
	isolateEnvironmentExport(t)
	cfg, err := loadAdmissionYAML(t, "defaults: &pool\n  max_conns: 8\ndatabase:\n  <<: *pool\n")
	// The standard YAML merge semantics must also apply to the environment projection.
	if err != nil {
		t.Fatal(err)
	}
	// Merged configuration is just as explicit as a directly declared field.
	if err := cfg.ExportEnvironment(); err != nil {
		t.Fatal(err)
	}
	// Runtime database consumers must receive merged pool settings.
	if os.Getenv("FUSED_DATABASE_MAX_CONNS") != "8" {
		t.Fatal("merged YAML setting was lost")
	}
}
