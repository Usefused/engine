package store

import (
	"strings"
	"testing"
)

// TestWorkspaceNotificationDedupeSQL_HasRegistryChangelogBranch is a pure
// query-shape assertion (no DB needed, same convention as
// workspace_webhook_store_test.go's TestUpsertWorkspaceWebhookSQLNeverReassignsSlug):
// Phase 3 (plans/plan-service-changelog.md) widened the dedupe predicate to
// key on metadata->>'registry_changelog_id' when present, using a jsonb `?`
// key-existence check so it can never accidentally match a pre-Phase-3 row
// that lacks the key.
func TestWorkspaceNotificationDedupeSQL_HasRegistryChangelogBranch(t *testing.T) {
	required := []string{
		`? 'registry_changelog_id'`,
		`metadata->>'registry_changelog_id' = ($7::jsonb)->>'registry_changelog_id'`,
	}
	for _, expected := range required {
		if !strings.Contains(workspaceNotificationDedupeMatchSQL, expected) {
			t.Fatalf("expected dedupe SQL containing %q: %s", expected, workspaceNotificationDedupeMatchSQL)
		}
	}
}

// TestWorkspaceNotificationDedupeSQL_FallsBackToPlanActionBranch proves the
// registry_changelog_id branch is additive: every pre-Phase-3 caller (the
// workspace_service_removed/workspace_version_removed apply-plan-action
// flow) has no registry_changelog_id key in its metadata at all, so it must
// still fall through to the original plan_id+action_id predicate,
// byte-for-byte.
func TestWorkspaceNotificationDedupeSQL_FallsBackToPlanActionBranch(t *testing.T) {
	required := []string{
		`NOT (($7::jsonb) ? 'registry_changelog_id')`,
		`metadata->>'plan_id' = ($7::jsonb)->>'plan_id'`,
		`COALESCE(metadata->>'action_id', '') = COALESCE(($7::jsonb)->>'action_id', '')`,
	}
	for _, expected := range required {
		if !strings.Contains(workspaceNotificationDedupeMatchSQL, expected) {
			t.Fatalf("expected dedupe SQL containing %q: %s", expected, workspaceNotificationDedupeMatchSQL)
		}
	}
}

// TestWorkspaceNotificationDedupeSQL_SelfParenthesized protects the shared
// predicate's composability: CreateWorkspaceNotification string-concatenates
// workspaceNotificationDedupeMatchSQL into two different places in the same
// WITH/UNION ALL query (the INSERT's NOT EXISTS guard and the SELECT-existing
// fallback). If the predicate weren't fully self-parenthesized, splicing an
// OR-containing expression into a larger `AND ...` chain could silently
// change operator precedence at either site.
func TestWorkspaceNotificationDedupeSQL_SelfParenthesized(t *testing.T) {
	trimmed := strings.TrimSpace(workspaceNotificationDedupeMatchSQL)
	if !strings.HasPrefix(trimmed, "(") || !strings.HasSuffix(trimmed, ")") {
		t.Fatalf("expected workspaceNotificationDedupeMatchSQL to be self-parenthesized so it composes safely: %s", workspaceNotificationDedupeMatchSQL)
	}
}
