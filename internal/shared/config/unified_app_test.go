package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadAdmissionYAML exercises the real loader, including defaults and process overrides.
func loadAdmissionYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.yaml")
	// Temporary configuration never contains credentials or alters a user's Engine configuration.
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestUnifiedAppYAMLDefaultsAndPrecedence verifies partial YAML preserves defaults and environment wins explicitly.
func TestUnifiedAppYAMLDefaultsAndPrecedence(t *testing.T) {
	cfg, err := loadAdmissionYAML(t, "engine:\n  unified_apps:\n    max_concurrency: 3\n")
	// Missing queue fields inherit defaults instead of decoding as disabling zero values.
	if err != nil {
		t.Fatal(err)
	}
	expected := UnifiedAppAdmissionConfig{MaxConcurrency: 3, MaxWorkerConcurrency: 4, QueueCapacity: 256, QueueTimeoutSeconds: 5}
	if cfg.Engine.UnifiedApps != expected {
		t.Fatalf("got %+v", cfg.Engine.UnifiedApps)
	}
	t.Setenv("FUSED_UNIFIED_APP_MAX_CONCURRENCY", "2")
	cfg, err = loadAdmissionYAML(t, "engine:\n  unified_apps:\n    max_concurrency: 3\n    queue_capacity: 99\n    queue_timeout_seconds: 8\n")
	// One process override must not erase neighboring YAML settings.
	if err != nil {
		t.Fatal(err)
	}
	expected = UnifiedAppAdmissionConfig{MaxConcurrency: 2, MaxWorkerConcurrency: 4, QueueCapacity: 99, QueueTimeoutSeconds: 8}
	if cfg.Engine.UnifiedApps != expected {
		t.Fatalf("got %+v", cfg.Engine.UnifiedApps)
	}
}

// TestUnifiedAppYAMLValidation rejects explicit zero, invalid types, and unsafe upper bounds at load time.
func TestUnifiedAppYAMLValidation(t *testing.T) {
	for _, body := range []string{
		"max_concurrency: 0", "max_concurrency: -1", "max_concurrency: 1025", "max_concurrency: many",
		"max_worker_concurrency: 0", "max_worker_concurrency: -1", "max_worker_concurrency: 1025",
		"queue_capacity: 0", "queue_capacity: 65537", "queue_timeout_seconds: 0", "queue_timeout_seconds: 301",
	} {
		_, err := loadAdmissionYAML(t, "engine:\n  unified_apps:\n    "+body+"\n")
		// Bad operator input must fail before any worker or server is created.
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

// TestUnifiedAppEnvironmentValidation confirms malformed and empty overrides never fall back silently.
func TestUnifiedAppEnvironmentValidation(t *testing.T) {
	for _, name := range []string{"FUSED_UNIFIED_APP_MAX_WORKER_CONCURRENCY", "FUSED_UNIFIED_APP_MAX_CONCURRENCY", "FUSED_UNIFIED_APP_QUEUE_CAPACITY", "FUSED_UNIFIED_APP_QUEUE_TIMEOUT_SECONDS"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"", "many", "0", "-1"} {
				t.Setenv(name, value)
				_, err := loadAdmissionYAML(t, "engine: {}\n")
				// The selected setting should be identifiable without exposing unrelated configuration.
				if err == nil || !(strings.Contains(err.Error(), "FUSED_UNIFIED_APP_") || strings.Contains(err.Error(), "engine.unified_apps.")) {
					t.Fatalf("invalid override %q: %v", value, err)
				}
			}
		})
	}
}

// TestUnifiedAppAbsentYAMLUsesCPUAwareDefaults preserves the prior environment-only deployment behavior.
func TestUnifiedAppAbsentYAMLUsesCPUAwareDefaults(t *testing.T) {
	cfg, err := loadAdmissionYAML(t, "engine: {}\n")
	// Existing Engine files remain usable without adding the new section.
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine.UnifiedApps != DefaultUnifiedAppAdmissionConfig() {
		t.Fatalf("unexpected defaults: %+v", cfg.Engine.UnifiedApps)
	}
}

// TestUnifiedAppWorkerCapacityConfiguration verifies YAML, export and environment all reach the per-worker limit.
func TestUnifiedAppWorkerCapacityConfiguration(t *testing.T) {
	cfg, err := loadAdmissionYAML(t, "engine:\n  unified_apps:\n    max_worker_concurrency: 16\n")
	// Higher capacity is explicit; the deployment-wide budget remains an independent setting.
	if err != nil || cfg.Engine.UnifiedApps.MaxWorkerConcurrency != 16 {
		t.Fatalf("worker capacity load: %v", err)
	}
	t.Setenv("FUSED_UNIFIED_APP_MAX_WORKER_CONCURRENCY", "32")
	cfg, err = loadAdmissionYAML(t, "engine:\n  unified_apps:\n    max_worker_concurrency: 16\n")
	// Environment overrides must win without changing the mounted YAML.
	if err != nil || cfg.Engine.UnifiedApps.MaxWorkerConcurrency != 32 {
		t.Fatalf("worker capacity override: %v", err)
	}
}
