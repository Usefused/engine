package store

import (
	"strings"
	"testing"
)

func TestListWorkspaceServicesQueryNormalizesProviderQualifiedReferencesInSQL(t *testing.T) {
	query, args := listWorkspaceServicesQuery([]string{"@acme/github"})
	for _, fragment := range []string{"unnest($1::text[])", "split_part(input.key, '/', 2)", "s.service_slug"} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q: %s", fragment, query)
		}
	}
	if len(args) != 1 {
		t.Fatalf("args = %#v, want one batched reference array", args)
	}
}
