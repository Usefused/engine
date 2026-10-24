package api

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine/mcpskill"
)

// fakeMCPSkillPolisher returns fixed prose for plan-time tests.
type fakeMCPSkillPolisher struct{ request mcpskill.PolishRequest }

// PolishMCPSkill records the evidence and returns safe prose.
func (f *fakeMCPSkillPolisher) PolishMCPSkill(_ context.Context, request mcpskill.PolishRequest) (mcpskill.Prose, error) {
	f.request = request
	return mcpskill.Prose{Purpose: "Handle customer support end to end."}, nil
}

// TestPlanMCPSkillPinsExactScopeAndProse verifies plan time groups the exact catalogue by service and attaches AI prose.
func TestPlanMCPSkillPinsExactScopeAndProse(t *testing.T) {
	fixture := newMCPAppOperationsGraphQLFixture()
	polisher := &fakeMCPSkillPolisher{}
	mcpskill.SetPolisher(polisher)
	t.Cleanup(func() { mcpskill.SetPolisher(nil) })
	resolved := []sdkResolvedService{
		{ServiceID: fixture.item.Selections[0].ServiceID, ServiceName: "crm", Version: "v2"},
		{ServiceID: fixture.item.Selections[1].ServiceID, ServiceName: "helpdesk", Version: "v1"},
	}
	snapshot := planMCPSkill(context.Background(), fixture, "support", "Manage tickets.", fixture.item.Selections, resolved)
	// The pinned inventory is grouped by authored service name with exact operation IDs.
	if snapshot == nil || len(snapshot.Services) != 2 || snapshot.Services[0].Label != "crm" || snapshot.Services[0].Operations[0] != "customers.list" || snapshot.Services[1].Operations[0] != "tickets.create" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	// AI prose is attached, and the model saw only names, description and operation IDs.
	if snapshot.Prose == nil || snapshot.Prose.Purpose != "Handle customer support end to end." || polisher.request.Name != "support" || len(polisher.request.Services) != 2 {
		t.Fatalf("prose = %#v, request = %#v", snapshot.Prose, polisher.request)
	}
}

// TestPlanMCPSkillWithoutEngineOptInNeverPolishes verifies no prose is requested unless the operator enabled engine.mcp_skill_polish.
func TestPlanMCPSkillWithoutEngineOptInNeverPolishes(t *testing.T) {
	mcpskill.SetPolisher(nil)
	fixture := newMCPAppOperationsGraphQLFixture()
	snapshot := planMCPSkill(context.Background(), fixture, "support", "Manage tickets.", fixture.item.Selections, nil)
	// The exact inventory is still pinned, but the template prose is used.
	if snapshot == nil || len(snapshot.Services) != 2 || snapshot.Prose != nil || mcpskill.PolishEnabled() {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
