package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Usefused/engine/internal/shared/config"
)

// TestStartCommandHasNoExternalUIOverride ensures operators cannot re-enable
// cross-origin UI hosting through the retired hidden flag.
func TestStartCommandHasNoExternalUIOverride(t *testing.T) {
	if flag := startCmd.Flags().Lookup("ui-url"); flag != nil {
		t.Fatal("Engine start command still exposes --ui-url")
	}
}

func TestLoadEngineEnvKeepsDotEnvSeparateFromInheritedEnvironment(t *testing.T) {
	preserveStartFlagValues(t)
	t.Setenv("FUSED_LICENSE_KEY", "environment-license-key")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("FUSED_LICENSE_KEY=dotenv-license-key\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	sources := loadEngineEnvFiles([]string{path})

	if sources.DotEnv != "dotenv-license-key" || sources.Environment != "environment-license-key" {
		t.Fatalf("license sources = %#v", sources)
	}
	if actual := os.Getenv("FUSED_LICENSE_KEY"); actual != "environment-license-key" {
		t.Fatalf("environment changed while reading .env: %q", actual)
	}
}

func TestApplyEngineOverridesUsesYAMLSecrets(t *testing.T) {
	preserveStartFlagValues(t)
	t.Setenv("FUSED_LICENSE_KEY", "")
	t.Setenv("FUSED_ENCRYPTION_KEY", "")

	cfg := &config.Config{EncryptionKey: "yaml-encryption-key"}
	cfg.Engine.LicenseKey = "yaml-license-key"
	applyEngineOverrides(cfg)

	assertEnvValue(t, "FUSED_LICENSE_KEY", "yaml-license-key")
	assertEnvValue(t, "FUSED_ENCRYPTION_KEY", "yaml-encryption-key")
}

func TestApplyEngineOverridesIgnoresFusedAPIKey(t *testing.T) {
	preserveStartFlagValues(t)
	t.Setenv("FUSED_API_KEY", "control-plane-key")
	t.Setenv("FUSED_LICENSE_KEY", "")

	applyEngineOverrides(&config.Config{})

	assertEnvValue(t, "FUSED_LICENSE_KEY", "")
}

// preserveStartFlagValues isolates process-global Cobra values between tests.
func preserveStartFlagValues(t *testing.T) {
	t.Helper()
	previousLicenseKey := licenseKey
	licenseKey = ""
	t.Cleanup(func() {
		licenseKey = previousLicenseKey
	})
}

func assertEnvValue(t *testing.T, name, expected string) {
	t.Helper()
	if actual := os.Getenv(name); actual != expected {
		t.Fatalf("%s = %q, want %q", name, actual, expected)
	}
}
