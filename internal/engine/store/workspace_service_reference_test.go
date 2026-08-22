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

func TestAppTokenBindingReusesWorkspaceServiceReferenceSemantics(t *testing.T) {
	// Token issuance must resolve every repeated binding inside its transaction
	// without inventing a narrower slug grammar than workspace service commands.
	wantResolver := workspaceServiceResolutionSQL(`requested input`, false)
	if !strings.Contains(insertAppTokenBindingsSQL, wantResolver) {
		t.Fatal("app token binding query does not reuse workspace service resolution")
	}
	for _, fragment := range []string{"SELECT DISTINCT key", "input.key LIKE '@%/%'", "split_part(input.key, '/', 2)", "priority_matches = 1"} {
		if !strings.Contains(insertAppTokenBindingsSQL, fragment) {
			t.Fatalf("app token binding query missing %q", fragment)
		}
	}
	if strings.Contains(wantResolver, "service.service_name") {
		t.Fatal("token service_slug resolution accepts a display name")
	}
}

func TestAppTokenBindingTreatsQualifiedAndBareSlugAsOneService(t *testing.T) {
	// Resolved pair counts prevent two public aliases for one service/auth pair
	// from reaching the unique binding constraint. The requested/persisted count
	// check then rejects the complete token transaction as invalid.
	for _, fragment := range []string{
		"COUNT(*) OVER (PARTITION BY token_id, service_id, auth_name) AS pair_count",
		"FROM resolved_counts WHERE pair_count = 1",
		"requested), (SELECT COUNT(*) FROM persisted)",
	} {
		if !strings.Contains(insertAppTokenBindingsSQL, fragment) {
			t.Fatalf("app token binding query missing alias-collapse guard %q", fragment)
		}
	}
}

func TestAppTokenFixedBindingRequiresValidPublicSlugAndAtLeastOneTuple(t *testing.T) {
	if err := validateAppTokenBindingRequests(AppTokenBindingFixed, nil); err == nil {
		t.Fatal("accepted fixed mode without a binding")
	}
	for _, reference := range []string{"", "@provider/", "@/service", "provider/service", "de305d54-75b4-431b-adb2-eb6b9e546014"} {
		err := validateAppTokenBindingRequests(AppTokenBindingFixed, []AppTokenBindingRequest{{
			ServiceSlug: reference, AuthName: "oauth2", EndUserRef: "customer",
		}})
		if err == nil {
			t.Fatalf("accepted invalid service slug reference %q", reference)
		}
	}
}
