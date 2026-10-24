package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPSkillPolishIsOffByDefaultAndOperatorControlled covers the default, YAML, env override and a rejected typo.
func TestMCPSkillPolishIsOffByDefaultAndOperatorControlled(t *testing.T) {
	t.Setenv("FUSED_ENGINE_MCP_SKILL_POLISH", "")
	defaults, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	// Without configuration no MCP metadata may leave the Engine.
	if err != nil || defaults.Engine.MCPSkillPolish {
		t.Fatalf("default = %v, err = %v", defaults.Engine.MCPSkillPolish, err)
	}
	path := filepath.Join(t.TempDir(), "engine.yaml")
	// The YAML key is the documented operator opt-in.
	if err := os.WriteFile(path, []byte("engine:\n  mcp_skill_polish: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configured, err := Load(path)
	if err != nil || !configured.Engine.MCPSkillPolish {
		t.Fatalf("yaml opt-in = %v, err = %v", configured.Engine.MCPSkillPolish, err)
	}
	t.Setenv("FUSED_ENGINE_MCP_SKILL_POLISH", "false")
	overridden, err := Load(path)
	// The environment wins over YAML, including to withdraw consent.
	if err != nil || overridden.Engine.MCPSkillPolish {
		t.Fatalf("env override = %v, err = %v", overridden.Engine.MCPSkillPolish, err)
	}
	t.Setenv("FUSED_ENGINE_MCP_SKILL_POLISH", "yes please")
	// An unparseable consent value stops startup instead of guessing.
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "FUSED_ENGINE_MCP_SKILL_POLISH") {
		t.Fatalf("invalid env err = %v", err)
	}
}
