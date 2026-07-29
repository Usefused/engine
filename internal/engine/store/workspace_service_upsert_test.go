package store

import (
	"strings"
	"testing"
)

func TestUpsertWorkspaceServicePreservesExistingDisplayMetadata(t *testing.T) {
	required := []string{
		"VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4)",
		"service_slug = COALESCE(EXCLUDED.service_slug, fused_workspace_services.service_slug)",
		"service_name = COALESCE(EXCLUDED.service_name, fused_workspace_services.service_name)",
	}
	for _, fragment := range required {
		if !strings.Contains(upsertWorkspaceServiceSQL, fragment) {
			t.Fatalf("workspace service upsert must preserve metadata with %q", fragment)
		}
	}
}
