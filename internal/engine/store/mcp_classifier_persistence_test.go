package store

import "testing"

// TestMCPClassifierPersistsInExactAppliedPlan verifies the production SQL projection after an atomic app apply.
func TestMCPClassifierPersistsInExactAppliedPlan(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeMCP)
	fixture.params.Scope.FusedIntelligentClassifier = true
	fixture.params.TokenHash = "classifier-test-token-hash"
	_, err := fixture.pool.Exec(fixture.ctx, `UPDATE fused_config_plans SET resolved_payload = resolved_payload || '{"fused-intelligent-classifier":true}'::jsonb WHERE id=$1`, fixture.params.Plan.PlanID)
	// The fixture uses an immutable resolved payload rather than an extra runtime column.
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	// Runtime loading must occur only after the canonical transaction marks the plan applied.
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewPostgresStore(fixture.pool).GetAppRuntime(fixture.ctx, fixture.params.Scope.AppID)
	// Version consent must survive process-independent database retrieval.
	if err != nil || runtime.FusedIntelligentClassifier != fixture.params.Scope.FusedIntelligentClassifier {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
}
