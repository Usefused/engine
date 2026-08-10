package workspaceplan

import "testing"

func TestEveryWorkspaceActionHasAuthorizationClass(t *testing.T) {
	if len(actionAuthorization) != 20 {
		t.Fatalf("registered action count = %d, want 20", len(actionAuthorization))
	}
	for action, class := range actionAuthorization {
		if action == "" || class == AuthorizationUnknown || !action.Valid() {
			t.Fatalf("invalid workspace action registration: action=%q class=%d", action, class)
		}
	}
	if ActionType("unknown_action").Valid() {
		t.Fatal("unknown workspace action must fail closed")
	}
}
