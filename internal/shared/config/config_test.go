package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestConfigHasNoExternalUIOrigin prevents environment/YAML wiring from
// returning through a differently named field after Engine becomes same-origin.
func TestConfigHasNoExternalUIOrigin(t *testing.T) {
	configType := reflect.TypeOf(Config{})
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		if field.Name == "UIURL" || field.Tag.Get("yaml") == "ui_url" {
			t.Fatalf("external UI origin remains configurable through %s", field.Name)
		}
	}
}

// TestLoad_HomepageURL_DefaultsWhenNoFile keeps the shared homepage default
// stable without reintroducing an Engine-specific external UI origin.
func TestLoad_HomepageURL_DefaultsWhenNoFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.HomepageURL != "http://localhost:3000" {
		t.Errorf("expected default HomepageURL %q, got %q", "http://localhost:3000", cfg.HomepageURL)
	}
}

// TestLoad_HomepageURL_OverriddenByYAML verifies shared configuration can
// still override its Registry-oriented homepage value independently.
func TestLoad_HomepageURL_OverriddenByYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	yamlContent := "homepage_url: \"https://usefused.com\"\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("failed to write test config file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.HomepageURL != "https://usefused.com" {
		t.Errorf("expected HomepageURL %q, got %q", "https://usefused.com", cfg.HomepageURL)
	}
}

func TestLoadEngineLicenseResolutionOrder(t *testing.T) {
	configuredPath := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(configuredPath, []byte("engine:\n  license_key: yaml-license-key\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	missingPath := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("FUSED_API_KEY", "must-not-be-used-by-engine")
	t.Setenv("FUSED_LICENSE_KEY", "ambient-value-must-not-bypass-explicit-sources")

	tests := []struct {
		name    string
		path    string
		sources EngineLicenseSources
		wantKey string
		wantSrc string
	}{
		{name: "flag", path: configuredPath, sources: EngineLicenseSources{Flag: "flag-key", DotEnv: "dotenv-key", Environment: "env-key"}, wantKey: "flag-key", wantSrc: "flag"},
		{name: "dotenv", path: configuredPath, sources: EngineLicenseSources{DotEnv: "dotenv-key", Environment: "env-key"}, wantKey: "dotenv-key", wantSrc: "dotenv"},
		{name: "yaml", path: configuredPath, sources: EngineLicenseSources{Environment: "env-key"}, wantKey: "yaml-license-key", wantSrc: "yaml"},
		{name: "environment fallback", path: missingPath, sources: EngineLicenseSources{Environment: "env-key"}, wantKey: "env-key", wantSrc: "environment"},
		{name: "missing ignores FUSED_API_KEY", path: missingPath, sources: EngineLicenseSources{}, wantSrc: "missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(test.path, WithEngineLicenseSources(test.sources))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Engine.LicenseKey != test.wantKey || cfg.Engine.LicenseKeySource != test.wantSrc {
				t.Fatalf("license = %q from %q, want %q from %q", cfg.Engine.LicenseKey, cfg.Engine.LicenseKeySource, test.wantKey, test.wantSrc)
			}
		})
	}
}

func TestLoadDefaultLicenseEnvironmentStillOverridesYAML(t *testing.T) {
	t.Setenv("FUSED_LICENSE_KEY", "environment-license-key")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("engine:\n  license_key: yaml-license-key\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Engine.LicenseKey != "environment-license-key" || cfg.Engine.LicenseKeySource != "environment" {
		t.Fatalf("default license resolution = %q from %q", cfg.Engine.LicenseKey, cfg.Engine.LicenseKeySource)
	}
}

func TestLoadExecutionRetentionDefaultsAndYAMLOverrides(t *testing.T) {
	defaults, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if defaults.Engine.ExecutionRetentionDays != 30 || defaults.Engine.ExecutionCleanupBatch != 1000 {
		t.Fatalf("retention defaults = %d days/%d rows", defaults.Engine.ExecutionRetentionDays, defaults.Engine.ExecutionCleanupBatch)
	}

	path := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(path, []byte("engine:\n  execution_retention_days: 7\n  execution_cleanup_batch: 250\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	configured, err := Load(path)
	if err != nil {
		t.Fatalf("Load configured: %v", err)
	}
	if configured.Engine.ExecutionRetentionDays != 7 || configured.Engine.ExecutionCleanupBatch != 250 {
		t.Fatalf("retention config = %d days/%d rows", configured.Engine.ExecutionRetentionDays, configured.Engine.ExecutionCleanupBatch)
	}
}

// TestLoadConnectedAuthRefreshWorkersDefaultAndYAML verifies the operator can
// select bounded OAuth refresh concurrency through Engine YAML.
func TestLoadConnectedAuthRefreshWorkersDefaultAndYAML(t *testing.T) {
	t.Setenv("FUSED_ENGINE_CONNECTED_AUTH_REFRESH_WORKERS", "")
	defaults, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if defaults.Engine.ConnectedAuthRefreshWorkers != DefaultConnectedAuthRefreshWorkers {
		t.Fatalf("default connected auth workers = %d, want %d", defaults.Engine.ConnectedAuthRefreshWorkers, DefaultConnectedAuthRefreshWorkers)
	}

	path := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(path, []byte("engine:\n  connected_auth_refresh_workers: 12\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	configured, err := Load(path)
	if err != nil {
		t.Fatalf("Load YAML workers: %v", err)
	}
	if configured.Engine.ConnectedAuthRefreshWorkers != 12 {
		t.Fatalf("YAML connected auth workers = %d, want 12", configured.Engine.ConnectedAuthRefreshWorkers)
	}
}

// TestLoadConnectedAuthRefreshWorkersEnvironment verifies the documented
// process override takes precedence over Engine YAML.
func TestLoadConnectedAuthRefreshWorkersEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(path, []byte("engine:\n  connected_auth_refresh_workers: 12\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FUSED_ENGINE_CONNECTED_AUTH_REFRESH_WORKERS", "7")

	configured, err := Load(path)
	if err != nil {
		t.Fatalf("Load environment workers: %v", err)
	}
	if configured.Engine.ConnectedAuthRefreshWorkers != 7 {
		t.Fatalf("environment connected auth workers = %d, want 7", configured.Engine.ConnectedAuthRefreshWorkers)
	}
}

// TestLoadRejectsInvalidConnectedAuthRefreshWorkers ensures malformed and
// unsafe concurrency settings fail startup instead of silently changing value.
func TestLoadRejectsInvalidConnectedAuthRefreshWorkers(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		envValue string
	}{
		{name: "zero YAML", yaml: "engine:\n  connected_auth_refresh_workers: 0\n"},
		{name: "negative YAML", yaml: "engine:\n  connected_auth_refresh_workers: -1\n"},
		{name: "excessive YAML", yaml: "engine:\n  connected_auth_refresh_workers: 65\n"},
		{name: "malformed environment", yaml: "engine:\n  connected_auth_refresh_workers: 4\n", envValue: "many"},
		{name: "zero environment", yaml: "engine:\n  connected_auth_refresh_workers: 4\n", envValue: "0"},
		{name: "excessive environment", yaml: "engine:\n  connected_auth_refresh_workers: 4\n", envValue: "65"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FUSED_ENGINE_CONNECTED_AUTH_REFRESH_WORKERS", test.envValue)
			path := filepath.Join(t.TempDir(), "engine.yaml")
			if err := os.WriteFile(path, []byte(test.yaml), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted invalid connected auth refresh workers")
			}
		})
	}
}

func TestLoadEnginePublicEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(path, []byte("engine:\n  public_url: https://yaml.example.com\n  public_grpc_url: https://yaml-grpc.example.com:443\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	configured, err := Load(path)
	if err != nil {
		t.Fatalf("Load YAML endpoints: %v", err)
	}
	if configured.Engine.PublicURL != "https://yaml.example.com" || configured.Engine.PublicGRPCURL != "https://yaml-grpc.example.com:443" {
		t.Fatalf("YAML public endpoints = %q/%q", configured.Engine.PublicURL, configured.Engine.PublicGRPCURL)
	}

	t.Setenv("FUSED_ENGINE_PUBLIC_URL", "https://env.example.com")
	t.Setenv("FUSED_ENGINE_PUBLIC_GRPC_URL", "https://env-grpc.example.com:443")
	configured, err = Load(path)
	if err != nil {
		t.Fatalf("Load environment endpoints: %v", err)
	}
	if configured.Engine.PublicURL != "https://env.example.com" || configured.Engine.PublicGRPCURL != "https://env-grpc.example.com:443" {
		t.Fatalf("environment public endpoints = %q/%q", configured.Engine.PublicURL, configured.Engine.PublicGRPCURL)
	}
}

// TestLoadEngineRegistryEndpoints verifies that registry_endpoint targets an optional
// Enterprise Local Registry while Fused Cloud Registry stays static unless explicitly overridden.
func TestLoadEngineRegistryEndpoints(t *testing.T) {
	t.Setenv("FUSED_REGISTRY_ENDPOINT", "")
	t.Setenv("FUSED_CLOUD_REGISTRY_ENDPOINT", "")

	defaults, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	// Missing YAML must keep Local Registry disabled and preserve Fused's static Cloud Registry URL.
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if defaults.Engine.RegistryEndpoint != "" {
		t.Fatalf("default local registry endpoint = %q, want empty", defaults.Engine.RegistryEndpoint)
	}
	if defaults.Engine.EffectiveCloudRegistryEndpoint() != DefaultCloudRegistryEndpoint {
		t.Fatalf("default cloud registry endpoint = %q, want %q", defaults.Engine.EffectiveCloudRegistryEndpoint(), DefaultCloudRegistryEndpoint)
	}

	path := filepath.Join(t.TempDir(), "engine.yaml")
	yamlContent := "engine:\n  registry_endpoint: https://local-registry.internal:8080\n  cloud_registry_endpoint: http://127.0.0.1:8090/graphql\n"
	// YAML write failure indicates a broken test workspace rather than config validation behavior.
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	fromYAML, err := Load(path)
	// YAML settings should populate the Local Registry URL and optional dev/staging Cloud override.
	if err != nil {
		t.Fatalf("Load YAML endpoints: %v", err)
	}
	if fromYAML.Engine.RegistryEndpoint != "https://local-registry.internal:8080" {
		t.Fatalf("YAML local registry endpoint = %q", fromYAML.Engine.RegistryEndpoint)
	}
	if fromYAML.Engine.EffectiveCloudRegistryEndpoint() != "http://127.0.0.1:8090/graphql" {
		t.Fatalf("YAML cloud registry endpoint = %q", fromYAML.Engine.EffectiveCloudRegistryEndpoint())
	}

	t.Setenv("FUSED_REGISTRY_ENDPOINT", "https://env-local-registry.internal:8443")
	t.Setenv("FUSED_CLOUD_REGISTRY_ENDPOINT", "https://staging-registry.usefused.com/graphql")
	fromEnv, err := Load(path)
	// Process environment overrides must take precedence over YAML values for both registry endpoints.
	if err != nil {
		t.Fatalf("Load env endpoints: %v", err)
	}
	if fromEnv.Engine.RegistryEndpoint != "https://env-local-registry.internal:8443" {
		t.Fatalf("env local registry endpoint = %q", fromEnv.Engine.RegistryEndpoint)
	}
	if fromEnv.Engine.EffectiveCloudRegistryEndpoint() != "https://staging-registry.usefused.com/graphql" {
		t.Fatalf("env cloud registry endpoint = %q", fromEnv.Engine.EffectiveCloudRegistryEndpoint())
	}
}

// TestEffectiveLocalRegistryEndpointIgnoresLegacyCloudURL ensures existing deployed Engines
// carrying FUSED_REGISTRY_ENDPOINT=https://registry.usefused.com/graphql stay in Cloud-only mode.
func TestEffectiveLocalRegistryEndpointIgnoresLegacyCloudURL(t *testing.T) {
	tests := []struct {
		name  string
		cfg   EngineConfig
		want  string
	}{
		{name: "empty", cfg: EngineConfig{}, want: ""},
		{name: "legacy default cloud graphql url", cfg: EngineConfig{RegistryEndpoint: "https://registry.usefused.com/graphql"}, want: ""},
		{name: "legacy default cloud bare url", cfg: EngineConfig{RegistryEndpoint: "https://registry.usefused.com/"}, want: ""},
		{name: "legacy default cloud uppercase url", cfg: EngineConfig{RegistryEndpoint: "HTTPS://REGISTRY.USEFUSED.COM/GraphQL/"}, want: ""},
		{name: "matches custom cloud override", cfg: EngineConfig{RegistryEndpoint: "http://127.0.0.1:8090", CloudRegistryEndpoint: "http://127.0.0.1:8090/graphql"}, want: ""},
		{name: "distinct local registry url", cfg: EngineConfig{RegistryEndpoint: "https://local-registry.internal:8080"}, want: "https://local-registry.internal:8080"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Verify each legacy or custom endpoint resolves to the expected Local Registry target.
			if got := tc.cfg.EffectiveLocalRegistryEndpoint(); got != tc.want {
				t.Fatalf("EffectiveLocalRegistryEndpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}
