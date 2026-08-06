package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_HomepageURL_DefaultsWhenNoFile covers the no-config-file path
// (Load returns defaults when the path doesn't exist) -- must include the
// new HomepageURL default alongside the existing UIURL default.
func TestLoad_HomepageURL_DefaultsWhenNoFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.HomepageURL != "http://localhost:3000" {
		t.Errorf("expected default HomepageURL %q, got %q", "http://localhost:3000", cfg.HomepageURL)
	}
	if cfg.UIURL != "http://localhost:5173" {
		t.Errorf("expected default UIURL %q, got %q", "http://localhost:5173", cfg.UIURL)
	}
}

// TestLoad_HomepageURL_OverriddenByYAML covers the Registry's actual
// registry.yaml shape: homepage_url alongside ui_url, both overriding
// defaults.
func TestLoad_HomepageURL_OverriddenByYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	yamlContent := "ui_url: \"https://app.usefused.com\"\nhomepage_url: \"https://usefused.com\"\n"
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
	if cfg.UIURL != "https://app.usefused.com" {
		t.Errorf("expected UIURL %q, got %q", "https://app.usefused.com", cfg.UIURL)
	}
}

func TestLoad_UIURL_OverriddenByEnv(t *testing.T) {
	t.Setenv("FUSED_UI_URL", "https://engine.example.com")

	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.UIURL != "https://engine.example.com" {
		t.Errorf("expected UIURL from FUSED_UI_URL, got %q", cfg.UIURL)
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
