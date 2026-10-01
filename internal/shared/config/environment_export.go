package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlEnvironmentBindings is the single startup bridge into existing os.Getenv consumers.
// Credentials remain in environment variables or referenced files; license precedence is unchanged.
var yamlEnvironmentBindings = map[string]string{
	"database.url":                              "FUSED_DATABASE_URL",
	"database.max_conns":                        "FUSED_DATABASE_MAX_CONNS",
	"database.max_conn_idle_time":               "FUSED_DATABASE_MAX_CONN_IDLE_TIME",
	"server.http_port":                          "FUSED_ENGINE_HTTP_PORT",
	"server.grpc_host":                          "FUSED_ENGINE_GRPC_HOST",
	"server.grpc_port":                          "FUSED_ENGINE_GRPC_PORT",
	"server.webhook_port":                       "FUSED_ENGINE_WEBHOOK_PORT",
	"nats.url":                                  "NATS_URL",
	"nats.store_dir":                            "FUSED_NATS_STORE_DIR",
	"nats.rate_limit_replicas":                  "FUSED_NATS_RATE_LIMIT_REPLICAS",
	"nats.credentials_file":                     "NATS_CREDS_FILE",
	"nats.nkey_seed_file":                       "NATS_NKEY_SEED_FILE",
	"nats.tls.ca_file":                          "NATS_TLS_CA_FILE",
	"nats.tls.cert_file":                        "NATS_TLS_CERT_FILE",
	"nats.tls.key_file":                         "NATS_TLS_KEY_FILE",
	"nats.tls.server_name":                      "NATS_TLS_SERVER_NAME",
	"observability.environment":                 "FUSED_ENGINE_ENVIRONMENT",
	"observability.service_name":                "OTEL_SERVICE_NAME",
	"observability.otel_target":                 "OTEL_EXPORTER_OTLP_ENDPOINT",
	"observability.traces_endpoint":             "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"observability.metrics_endpoint":            "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"observability.logs_endpoint":               "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"engine.unified_apps.max_concurrency":       "FUSED_UNIFIED_APP_MAX_CONCURRENCY",
	"engine.unified_apps.queue_capacity":        "FUSED_UNIFIED_APP_QUEUE_CAPACITY",
	"engine.unified_apps.queue_timeout_seconds": "FUSED_UNIFIED_APP_QUEUE_TIMEOUT_SECONDS",
}

// yamlEnvironmentDefaults records only declared scalar values, including merged YAML mappings.
func yamlEnvironmentDefaults(document *yaml.Node) (map[string]string, error) {
	var values map[string]any
	// The standard decoder handles anchors and rejects recursive aliases before exporting anything.
	if err := document.Decode(&values); err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for path, name := range yamlEnvironmentBindings {
		value, exists := yamlValueAtPath(values, strings.Split(path, "."))
		// Missing and null YAML values must leave environment-only deployments untouched.
		if !exists || value == nil {
			continue
		}
		text := fmt.Sprint(value)
		// Validate every declared NATS replication value instead of exporting an invalid fallback silently.
		if path == "nats.rate_limit_replicas" {
			count, err := strconv.Atoi(text)
			// JetStream accepts only the documented deployment replication range.
			if err != nil || count < 1 || count > 5 {
				return nil, fmt.Errorf("nats.rate_limit_replicas must be between 1 and 5")
			}
		}
		// Legacy YAML targets may use host:port; the standard OTLP environment contract expects a URL.
		if path == "observability.otel_target" && text != "" && !strings.Contains(text, "://") {
			text = "http://" + text
		}
		// Reject invalid process values before any part of the environment is changed.
		if strings.ContainsRune(text, 0) {
			return nil, fmt.Errorf("%s contains a value that cannot be exported to the environment", path)
		}
		result[name] = text
	}
	return result, nil
}

// yamlValueAtPath distinguishes omitted keys from explicit zero, false, and empty scalar values.
func yamlValueAtPath(values map[string]any, path []string) (any, bool) {
	var current any = values
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		// A non-mapping parent cannot declare a nested operator setting.
		if !ok {
			return nil, false
		}
		current, ok = mapping[key]
		// Omitted YAML must never publish a default over an existing process setting.
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// ExportEnvironment fills absent process settings once at Engine startup; runtime consumers remain environment-based.
// This intentionally stays separate from Load so inspecting configuration never mutates the caller's process.
func (cfg *Config) ExportEnvironment() error {
	inherited := make(map[string]string)
	for _, name := range yamlEnvironmentBindings {
		inherited[name] = os.Getenv(name)
	}
	inherited["DATABASE_URL"] = os.Getenv("DATABASE_URL")
	for _, name := range []string{"NATS_TOKEN", "NATS_USERNAME", "NATS_PASSWORD"} {
		inherited[name] = os.Getenv(name)
	}
	for name, value := range cfg.environmentDefaults {
		// Existing process settings and equivalent higher-level sources keep their established priority.
		if skipYAMLEnvironmentDefault(name, inherited) {
			continue
		}
		// Environment publication errors include only the variable name, never credentials or values.
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("cannot export Engine setting %s", name)
		}
	}
	return nil
}

// skipYAMLEnvironmentDefault preserves precedence across aliases and grouped authentication settings.
func skipYAMLEnvironmentDefault(name string, inherited map[string]string) bool {
	// Empty variables historically behave as absent overrides throughout Engine configuration.
	if inherited[name] != "" {
		return true
	}
	// The legacy database alias still wins over a YAML connection string.
	if name == "FUSED_DATABASE_URL" && inherited["DATABASE_URL"] != "" {
		return true
	}
	// An inherited shared OTLP endpoint outranks YAML per-signal destinations.
	if name != "OTEL_EXPORTER_OTLP_ENDPOINT" && strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_") && inherited["OTEL_EXPORTER_OTLP_ENDPOINT"] != "" {
		return true
	}
	// Explicit environment authentication owns the entire method; never mix it with a YAML file method.
	if name == "NATS_CREDS_FILE" || name == "NATS_NKEY_SEED_FILE" {
		for _, auth := range []string{"NATS_CREDS_FILE", "NATS_NKEY_SEED_FILE", "NATS_TOKEN", "NATS_USERNAME", "NATS_PASSWORD"} {
			// Any selected environment method suppresses lower-priority YAML authentication files.
			if inherited[auth] != "" {
				return true
			}
		}
	}
	return false
}
