package store

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestApplyAppConfigPlanRejectsMalformedSelectionsBeforeCommit proves invalid runtime scope cannot leave app, token or config state.
func TestApplyAppConfigPlanRejectsMalformedSelectionsBeforeCommit(t *testing.T) {
	// Both app kinds share the same persistence admission invariant.
	for _, kind := range []ConfigType{ConfigTypeSDK, ConfigTypeMCP} {
		// Empty, missing, incomplete and outdated scopes must all fail before publication.
		for _, payload := range []string{`[]`, `null`, `[{}]`, `[{"schema_version":2}]`} {
			t.Run(string(kind)+payload, func(t *testing.T) {
				fixture := newConcurrentArtifactApplyFixture(t, kind)
				fixture.params.Scope.Selections = []byte(payload)
				_, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
				// The canonical decoder must reject before the transaction publishes anything.
				if !errors.Is(err, models.ErrAppSelectionSchemaMismatch) {
					t.Fatalf("invalid scope error: %v", err)
				}
				var count int
				err = fixture.pool.QueryRow(fixture.ctx, `SELECT (SELECT count(*) FROM fused_apps WHERE app_id=$1)+(SELECT count(*) FROM fused_config_states WHERE latest_resource_id=$1)+(SELECT count(*) FROM fused_config_plans WHERE id=$2 AND status='applied')`, fixture.params.Scope.AppID, fixture.params.Plan.PlanID).Scan(&count)
				// Absence of app/state and an unconsumed plan prove no partial publication occurred.
				if err != nil || count != 0 {
					t.Fatalf("partial invalid apply: count=%d err=%v", count, err)
				}
			})
		}
	}
}
