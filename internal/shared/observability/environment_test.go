package observability

import "testing"

// TestEngineEnvironment_DefaultsToProduction is Task 8's core AC
// (engine_workspace_registration_plan.md): existing single-Engine
// deployments that never set FUSED_ENGINE_ENVIRONMENT must see no behavior
// change, so the label defaults to "production".
func TestEngineEnvironment_DefaultsToProduction(t *testing.T) {
	t.Setenv(EngineEnvironmentEnvVar, "")

	if got := EngineEnvironment(); got != "production" {
		t.Errorf("expected default \"production\", got %q", got)
	}
}

// TestEngineEnvironment_ReadsEnvVar covers an operator-set label (e.g.
// "staging") flowing through to the resolved value.
func TestEngineEnvironment_ReadsEnvVar(t *testing.T) {
	t.Setenv(EngineEnvironmentEnvVar, "staging")

	if got := EngineEnvironment(); got != "staging" {
		t.Errorf("expected \"staging\", got %q", got)
	}
}
