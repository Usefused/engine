package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestUpsertWorkspaceWebhookSQLNeverReassignsSlug is a pure query-shape
// assertion (no DB needed): a registration's slug -- and therefore its URL
// -- must never change on a re-apply, even though every other field is
// refreshed. This is the one invariant a live-DB test can't cheaply prove
// wrong in a way that fails loudly, so it's pinned here as a string check.
func TestUpsertWorkspaceWebhookSQLNeverReassignsSlug(t *testing.T) {
	if strings.Contains(upsertWorkspaceWebhookSQL, "slug = EXCLUDED.slug") {
		t.Fatalf("upsert must never reassign slug on conflict -- an existing registration's URL must stay stable across re-applies: %s", upsertWorkspaceWebhookSQL)
	}
	if !strings.Contains(upsertWorkspaceWebhookSQL, "ON CONFLICT (service_id, label)") {
		t.Fatalf("upsert must key on (service_id, label): %s", upsertWorkspaceWebhookSQL)
	}
}

// TestWorkspaceWebhookStore groups the DB-backed integration tests for the
// new Engine-owned webhook registration store. Skipped when DATABASE_URL is
// unset, same convention as TestActivationStore in workspace_store_test.go.
//
//	DATABASE_URL=postgres://... go test ./internal/engine/store/...
func TestWorkspaceWebhookStore(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping workspace webhook store tests: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool)
	accountID := uuid.New()

	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("reset singleton workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspace_webhooks"); err != nil {
		t.Fatalf("reset webhooks: %v", err)
	}
	_, err = s.BootstrapWorkspace(ctx, accountID, "Webhook Store Test Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}

	t.Run("TestUpsertWorkspaceWebhook_CreatesAndKeepsSlugOnReapply", func(t *testing.T) {
		svcID := uuid.New()
		verID := uuid.New()

		created, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID:        svcID,
			ServiceVersionID: verID,
			Label:            "repo-a",
			Slug:             "aaaaaaaaaaaaaaaaaaaaa",
			AuthType:         "hmac_signature",
			SignatureHeader:  "X-Signature",
		})
		if err != nil {
			t.Fatalf("first UpsertWorkspaceWebhook: %v", err)
		}
		if created.Slug != "aaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("expected the candidate slug to win on first insert, got %q", created.Slug)
		}

		// Re-apply with a different candidate slug and an updated secret --
		// the slug must not move even though the secret does.
		updated, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID:        svcID,
			ServiceVersionID: verID,
			Label:            "repo-a",
			Slug:             "bbbbbbbbbbbbbbbbbbbbb",
			AuthType:         "hmac_signature",
			SignatureHeader:  "X-Signature",
		})
		if err != nil {
			t.Fatalf("second UpsertWorkspaceWebhook (re-apply): %v", err)
		}
		if updated.Slug != created.Slug {
			t.Fatalf("slug must survive a re-apply: got %q, want %q", updated.Slug, created.Slug)
		}
	})

	// TestUpsertWorkspaceWebhook_NilVerificationHeadersDoesNotViolateNotNull
	// guards the same class of footgun PruneWorkspaceWebhooks already guards
	// against: pgx encodes a nil []string as SQL NULL, but
	// verification_headers is `text[] NOT NULL DEFAULT '{}'` -- so a service
	// with no declared verification headers (the common case: no webhook
	// auth at all, or an auth_type like hmac_signature that doesn't use this
	// field) must not fail the upsert with a NOT NULL violation.
	t.Run("TestUpsertWorkspaceWebhook_NilVerificationHeadersDoesNotViolateNotNull", func(t *testing.T) {
		svcID := uuid.New()

		created, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID:       svcID,
			Label:           "no-verification-headers",
			Slug:            "ggggggggggggggggggggg",
			AuthType:        "hmac_signature",
			SignatureHeader: "X-Signature",
			// VerificationHeaders intentionally left nil.
		})
		if err != nil {
			t.Fatalf("UpsertWorkspaceWebhook with nil VerificationHeaders: %v", err)
		}
		if len(created.VerificationHeaders) != 0 {
			t.Fatalf("expected empty VerificationHeaders, got %#v", created.VerificationHeaders)
		}

		// Re-apply (the ON CONFLICT DO UPDATE path) must not violate the
		// constraint either.
		updated, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID:       svcID,
			Label:           "no-verification-headers",
			Slug:            created.Slug,
			AuthType:        "hmac_signature",
			SignatureHeader: "X-Signature",
		})
		if err != nil {
			t.Fatalf("re-apply UpsertWorkspaceWebhook with nil VerificationHeaders: %v", err)
		}
		if len(updated.VerificationHeaders) != 0 {
			t.Fatalf("expected empty VerificationHeaders on re-apply, got %#v", updated.VerificationHeaders)
		}
	})

	t.Run("TestGetWorkspaceWebhookBySlug_ResolvesAndMisses", func(t *testing.T) {
		svcID := uuid.New()
		created, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID: svcID,
			Label:     "prod",
			Slug:      "ddddddddddddddddddddd",
		})
		if err != nil {
			t.Fatalf("UpsertWorkspaceWebhook: %v", err)
		}

		got, err := s.GetWorkspaceWebhookBySlug(ctx, created.Slug)
		if err != nil {
			t.Fatalf("GetWorkspaceWebhookBySlug: %v", err)
		}
		if got.Label != "prod" {
			t.Fatalf("expected label %q, got %q", "prod", got.Label)
		}
		// The ingress lookup joins fused_workspaces to resolve AccountID --
		// downstream ingress code (NATS subject, analytics) needs it and this
		// must not require a second round trip.
		if got.AccountID != accountID {
			t.Fatalf("expected AccountID %s resolved via join, got %s", accountID, got.AccountID)
		}

		_, err = s.GetWorkspaceWebhookBySlug(ctx, "does-not-exist")
		if !errors.Is(err, ErrWorkspaceWebhookNotFound) {
			t.Fatalf("expected ErrWorkspaceWebhookNotFound for an unknown slug, got %v", err)
		}
	})

	t.Run("TestListWorkspaceWebhooks_ReturnsAllLabelsForService", func(t *testing.T) {
		svcID := uuid.New()
		for _, label := range []string{"repo-a", "repo-b", "staging"} {
			if _, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
				ServiceID: svcID,
				Label:     label,
				Slug:      label + "-slug-000000000000",
			}); err != nil {
				t.Fatalf("UpsertWorkspaceWebhook(%s): %v", label, err)
			}
		}

		list, err := s.ListWorkspaceWebhooks(ctx, svcID)
		if err != nil {
			t.Fatalf("ListWorkspaceWebhooks: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("expected 3 registrations, got %d: %#v", len(list), list)
		}
	})

	t.Run("TestRemoveWorkspaceWebhook_DeletesOnlyThatLabel", func(t *testing.T) {
		svcID := uuid.New()
		if _, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID: svcID, Label: "keep", Slug: "eeeeeeeeeeeeeeeeeeeee",
		}); err != nil {
			t.Fatalf("UpsertWorkspaceWebhook(keep): %v", err)
		}
		if _, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
			ServiceID: svcID, Label: "remove", Slug: "fffffffffffffffffffff",
		}); err != nil {
			t.Fatalf("UpsertWorkspaceWebhook(remove): %v", err)
		}

		if err := s.RemoveWorkspaceWebhook(ctx, svcID, "remove"); err != nil {
			t.Fatalf("RemoveWorkspaceWebhook: %v", err)
		}

		list, err := s.ListWorkspaceWebhooks(ctx, svcID)
		if err != nil {
			t.Fatalf("ListWorkspaceWebhooks: %v", err)
		}
		if len(list) != 1 || list[0].Label != "keep" {
			t.Fatalf("expected only %q to remain, got %#v", "keep", list)
		}

		err = s.RemoveWorkspaceWebhook(ctx, svcID, "remove")
		if !errors.Is(err, ErrWorkspaceWebhookNotFound) {
			t.Fatalf("expected ErrWorkspaceWebhookNotFound removing an already-removed label, got %v", err)
		}
	})

	// TestPruneWorkspaceWebhooks_NilKeepLabelsRemovesEverything guards against
	// a real footgun: pgx encodes a nil []string parameter as SQL NULL, and
	// `label = ANY(NULL)` evaluates to NULL rather than false, which
	// `WHERE NOT (...)` then treats as "exclude this row" -- so passing a nil
	// keepLabels straight through to the query would silently prune nothing
	// instead of everything (the exact behavior a full-service-removal apply
	// relies on). This proves the nil-to-empty-slice normalization actually
	// deletes every registration, not just that the query doesn't error.
	t.Run("TestPruneWorkspaceWebhooks_NilKeepLabelsRemovesEverything", func(t *testing.T) {
		svcID := uuid.New()
		for _, label := range []string{"a", "b"} {
			if _, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
				ServiceID: svcID, Label: label, Slug: label + "-prune-0000000000000",
			}); err != nil {
				t.Fatalf("UpsertWorkspaceWebhook(%s): %v", label, err)
			}
		}

		removed, err := s.PruneWorkspaceWebhooks(ctx, svcID, nil)
		if err != nil {
			t.Fatalf("PruneWorkspaceWebhooks(nil): %v", err)
		}
		if len(removed) != 2 {
			t.Fatalf("expected both registrations removed by a nil keepLabels prune, got %#v", removed)
		}

		list, err := s.ListWorkspaceWebhooks(ctx, svcID)
		if err != nil {
			t.Fatalf("ListWorkspaceWebhooks: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("expected no registrations left after a nil-keepLabels prune, got %#v", list)
		}
	})

	// TestPruneWorkspaceWebhooks_KeepsOnlyListedLabels is the partial-prune
	// counterpart -- confirms a non-empty keepLabels leaves exactly those
	// registrations and removes the rest in one query.
	t.Run("TestPruneWorkspaceWebhooks_KeepsOnlyListedLabels", func(t *testing.T) {
		svcID := uuid.New()
		for _, label := range []string{"repo-a", "repo-b", "stale"} {
			if _, err := s.UpsertWorkspaceWebhook(ctx, WorkspaceWebhook{
				ServiceID: svcID, Label: label, Slug: label + "-partial-000000000000",
			}); err != nil {
				t.Fatalf("UpsertWorkspaceWebhook(%s): %v", label, err)
			}
		}

		removed, err := s.PruneWorkspaceWebhooks(ctx, svcID, []string{"repo-a", "repo-b"})
		if err != nil {
			t.Fatalf("PruneWorkspaceWebhooks: %v", err)
		}
		if len(removed) != 1 || removed[0] != "stale" {
			t.Fatalf("expected only %q pruned, got %#v", "stale", removed)
		}

		list, err := s.ListWorkspaceWebhooks(ctx, svcID)
		if err != nil {
			t.Fatalf("ListWorkspaceWebhooks: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("expected 2 registrations to remain, got %#v", list)
		}
	})
}
