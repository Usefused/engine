package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExecutionResultRetentionConfiguration keeps result lifetime operator-owned, including explicit indefinite retention.
func TestExecutionResultRetentionConfiguration(t *testing.T) {
	// An omitted field must differ from an explicit zero without requiring a second setting.
	for _, tc := range []struct {
		name, yaml string
		want       int
		invalid    bool
	}{
		{"default", "engine: {}", 7, false},
		{"long", "engine:\n  execution_result_retention_days: 3650", 3650, false},
		{"forever", "engine:\n  execution_result_retention_days: 0", 0, false},
		{"negative", "engine:\n  execution_result_retention_days: -1", 0, true},
	} {
		// Separate files keep loader defaults from leaking between cases.
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "engine.yaml")
			// Invalid fixture writes cannot establish a configuration assertion.
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			// Negative values must fail instead of silently deleting data on a guessed schedule.
			if tc.invalid {
				if err == nil {
					t.Fatal("negative retention accepted")
				}
				return
			}
			// Defaults, extended retention, and explicit zero must survive YAML loading exactly.
			if err != nil || cfg.Engine.ExecutionResultRetentionDays != tc.want {
				t.Fatalf("config=%+v error=%v", cfg, err)
			}
		})
	}
}
