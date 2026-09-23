package store

import "testing"

// TestAppApplyMetadataDeferredToken separates explicit no-token admission from accidentally incomplete issuance.
func TestAppApplyMetadataDeferredToken(t *testing.T) {
	params := ApplyAppConfigPlanParams{SkipTokenIssuance: true}
	params.Plan.State.ConfigType = ConfigTypeMCP
	// Deferring issuance must not require creating unused secret material.
	if err := validateAppApplyMetadata(params); err != nil {
		t.Fatal(err)
	}
	params.SkipTokenIssuance = false
	// Ordinary issuance still requires a complete credential identity.
	if err := validateAppApplyMetadata(params); err == nil {
		t.Fatal("missing token identity accepted")
	}
}

// TestApplyWorkflowAppWithoutToken verifies the real transaction publishes an MCP while leaving its token family empty.
func TestApplyWorkflowAppWithoutToken(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeMCP)
	params := fixture.params
	params.SkipTokenIssuance, params.TokenHash, params.TokenName = true, "", ""
	result, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, params)
	// A valid deferred-token apply must commit the same immutable app lifecycle as ordinary installation.
	if err != nil {
		t.Fatal(err)
	}
	// The response cannot claim a credential was created when the caller explicitly opted out.
	if result.TokenCreated {
		t.Fatal("deferred token was issued")
	}
	var apps, tokens int
	// Count both sides of the transaction to detect accidental partial publication or hidden token creation.
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT (SELECT count(*) FROM fused_apps WHERE app_id=$1), (SELECT count(*) FROM fused_app_tokens WHERE app_family_id=$2)`, result.AppID, result.AppFamilyID).Scan(&apps, &tokens); err != nil {
		t.Fatal(err)
	}
	// One runnable app with no token is the exact requested outcome.
	if apps != 1 || tokens != 0 {
		t.Fatalf("unexpected persisted app/token counts: %d/%d", apps, tokens)
	}
}
