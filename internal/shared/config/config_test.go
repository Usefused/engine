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
