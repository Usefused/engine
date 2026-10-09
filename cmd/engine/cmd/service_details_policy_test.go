package cmd

import (
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"net/http"
	"testing"
)

// TestServiceDetailsPolicyRequiresManagement prevents the shared edit route from bypassing local RBAC.
func TestServiceDetailsPolicyRequiresManagement(t *testing.T) {
	for _, policy := range controlRESTPolicies {
		// Only this exact mutation may authorize display metadata changes.
		if policy.method == http.MethodPatch && policy.pattern == "/integrations/{service_id}/details" {
			// Registry ownership supplements rather than replaces Engine management permission.
			if len(policy.requirements) != 1 || policy.requirements[0].permission != accesscontrol.PermissionServiceManage {
				t.Fatalf("unexpected requirements: %#v", policy.requirements)
			}
			return
		}
	}
	t.Fatal("service details mutation has no explicit authorization policy")
}
