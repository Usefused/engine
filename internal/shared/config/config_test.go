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
