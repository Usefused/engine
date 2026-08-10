package api

import (
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/workspaceplan"
)

func TestParseWorkspacePlanActionsUsesSharedVocabulary(t *testing.T) {
	actions, err := parseWorkspacePlanActions(json.RawMessage(`[{"id":"add:one","type":"add_service","service_id":"service"}]`))
	if err != nil {
		t.Fatalf("parseWorkspacePlanActions: %v", err)
	}
	if actions["add:one"].Type != workspaceplan.ActionAddService {
		t.Fatalf("action type = %q, want %q", actions["add:one"].Type, workspaceplan.ActionAddService)
	}
}

func TestParseWorkspacePlanActionsRejectsUnknownOrMissingIdentity(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"unknown type": json.RawMessage(`[{"id":"unknown:one","type":"unknown"}]`),
		"missing id":   json.RawMessage(`[{"type":"add_service"}]`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWorkspacePlanActions(raw); err == nil {
				t.Fatal("invalid workspace action was accepted")
			}
		})
	}
}
