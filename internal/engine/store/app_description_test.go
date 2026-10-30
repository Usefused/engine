package store

import (
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

// TestHistoricalAppDescriptionRecovery proves catalog and runtime reads repair exact-plan metadata without altering stored plans.
func TestHistoricalAppDescriptionRecovery(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	fixture.params.TokenHash = "description-test-" + fixture.params.Scope.AppID.String()
	_, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	// Only successfully published apps may expose immutable plan metadata.
	if err != nil {
		t.Fatal(err)
	}
	repository := NewPostgresStore(fixture.pool).(*postgresStore)
	for _, tc := range []struct{ name, stored, authored, want string }{
		{"authored", "GitOps managed SDK mac-control", "  Read Mac status.  ", "Read Mac status."},
		{"absent", "GitOps managed SDK mac-control", "", ""},
		{"custom", "A custom server description.", "Different app source description.", "A custom server description."},
		{"similar text", "GitOps managed SDK is our example", "Authored", "GitOps managed SDK is our example"},
	} {
		// Each fixture keeps desired metadata and the historical payload distinct, as the affected release did.
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]string{"name": "mac-control", "description": tc.stored})
			desired, _ := json.Marshal(map[string]string{"description": tc.authored})
			_, err := fixture.pool.Exec(fixture.ctx, `UPDATE fused_config_plans SET resolved_payload=$2, desired_state=$3 WHERE id=$1`, fixture.params.Plan.PlanID, payload, desired)
			// Fixture setup errors must not masquerade as successful recovery.
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := repository.GetAppRuntime(fixture.ctx, fixture.params.Scope.AppID)
			// Runtime identity and protocol metadata use the same correction as the catalogue.
			if err != nil || runtime.Description != tc.want {
				t.Fatalf("runtime=%+v error=%v want=%q", runtime, err, tc.want)
			}
			item, err := repository.GetAuthorizedApp(fixture.ctx, fixture.params.Scope.AccountID, fixture.params.Scope.AppID, accesscontrol.AuthorizedScope{All: true})
			// Detail reads cannot retain the placeholder after runtime identity has been repaired.
			if err != nil || item.Description != tc.want {
				t.Fatalf("catalogue=%+v error=%v want=%q", item, err, tc.want)
			}
			var stored string
			err = fixture.pool.QueryRow(fixture.ctx, `SELECT resolved_payload->>'description' FROM fused_config_plans WHERE id=$1`, fixture.params.Plan.PlanID).Scan(&stored)
			// Presentation repair must leave immutable generation and audit inputs byte-for-byte intact.
			if err != nil || stored != tc.stored {
				t.Fatalf("stored=%q error=%v", stored, err)
			}
		})
	}
}
