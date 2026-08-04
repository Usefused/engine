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

func TestAddWorkspaceServiceVersion_RejectsEmptyVersion(t *testing.T) {
	s := &postgresStore{}

	err := s.AddWorkspaceServiceVersion(context.Background(), uuid.New(), "", " ", uuid.Nil, "Stripe", uuid.New())
	if !errors.Is(err, ErrWorkspaceServiceVersionRequired) {
		t.Fatalf("expected ErrWorkspaceServiceVersionRequired, got %v", err)
	}
}

func TestEnableWorkspaceServiceVersion_RejectsEmptyVersion(t *testing.T) {
	s := &postgresStore{}

	err := s.EnableWorkspaceServiceVersion(context.Background(), uuid.New(), " ", uuid.Nil, uuid.New())
	if !errors.Is(err, ErrWorkspaceServiceVersionRequired) {
		t.Fatalf("expected ErrWorkspaceServiceVersionRequired, got %v", err)
	}
}

// TestListWorkspaceServiceVersionsForServices_EmptyInputSkipsQuery asserts the
// batched lookup short-circuits before touching the DB pool when there are
// no service IDs to look up -- exercised against a postgresStore with a nil
// pool, so this test would panic on a nil-pointer dereference if the
// short-circuit were ever removed, instead of just returning an empty map.
func TestListWorkspaceServiceVersionsForServices_EmptyInputSkipsQuery(t *testing.T) {
	s := &postgresStore{}

	got, err := s.ListWorkspaceServiceVersionsForServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %#v", got)
	}
}

func TestLatestWorkspaceServiceVersionQueryExcludesDeprecated(t *testing.T) {
	if !strings.Contains(latestWorkspaceServiceVersionSQL, "status <> 'deprecated'") {
		t.Fatalf("latest workspace version resolution must exclude deprecated versions: %s", latestWorkspaceServiceVersionSQL)
	}
	if !strings.Contains(latestWorkspaceServiceVersionSQL, "ORDER BY enabled_at DESC, id DESC") {
		t.Fatalf("latest workspace version resolution must use enablement order: %s", latestWorkspaceServiceVersionSQL)
	}
}

func TestWorkspaceServiceVersionByIDQueryUsesExactPinnedTuple(t *testing.T) {
	required := []string{
		"service_id = $1",
		"service_version_id = $2",
		"status <> 'deprecated'",
	}
	for _, fragment := range required {
		if !strings.Contains(workspaceServiceVersionByIDSQL, fragment) {
			t.Fatalf("exact workspace version lookup must contain %q: %s", fragment, workspaceServiceVersionByIDSQL)
		}
	}
}

func TestAuthorizedWorkspaceServiceFilterSupportsCLIReferences(t *testing.T) {
	for _, fragment := range []string{
		"s.service_name",
		"s.service_slug",
		"split_part(requested.name, '/', 2)",
	} {
		if !strings.Contains(authorizedWorkspaceServicesWhereSQL, fragment) {
			t.Fatalf("authorized workspace-service filter must contain %q: %s", fragment, authorizedWorkspaceServicesWhereSQL)
		}
	}
}

func TestMissingContractSnapshotsQueryUsesSQLAntiJoin(t *testing.T) {
	required := []string{
		"LEFT JOIN fused_service_contract_snapshots snapshots",
		"ON snapshots.service_version_id = versions.service_version_id",
		"snapshots.id IS NULL",
		"versions.status <> 'deprecated'",
		"LIMIT $1",
	}
	for _, fragment := range required {
		if !strings.Contains(workspaceServiceVersionsMissingContractSnapshotsSQL, fragment) {
			t.Fatalf("missing snapshot lookup must contain %q: %s", fragment, workspaceServiceVersionsMissingContractSnapshotsSQL)
		}
	}
}

// TestActivationStore groups the S2 store integration tests.
// All tests are skipped when DATABASE_URL is unset — they require a live
// Postgres instance with the Engine schema applied. Run with:
//
//	DATABASE_URL=postgres://... go test ./internal/engine/store/...
func TestActivationStore(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping activation store tests: DATABASE_URL not set")
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
	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspace_services"); err != nil {
		t.Fatalf("reset workspace services: %v", err)
	}

	// Every sub-test shares a fresh workspace so rows don't bleed between them.
	_, err = s.BootstrapWorkspace(ctx, accountID, "S2 Test Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}

	t.Run("TestActivateService_Idempotent", func(t *testing.T) {
		svcID := uuid.New()

		// First call — should succeed.
		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "1.0", uuid.Nil, "Stripe", accountID); err != nil {
			t.Fatalf("first AddWorkspaceServiceVersion: %v", err)
		}
		// Second identical call — must not error (ON CONFLICT DO UPDATE is safe).
		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "1.0", uuid.Nil, "Stripe", accountID); err != nil {
			t.Fatalf("second AddWorkspaceServiceVersion (idempotent): %v", err)
		}

		// After two inserts there should still be exactly one row.
		rows, err := s.ListWorkspaceServices(ctx, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceServices: %v", err)
		}
		count := 0
		for _, r := range rows {
			if r.ServiceID == svcID {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected 1 activation row for svc %s, got %d", svcID, count)
		}
	})

	t.Run("TestWorkspaceServiceVersions_StoresMultipleEnabledVersions", func(t *testing.T) {
		svcID := uuid.New()

		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "2026-07-09", uuid.New(), "Stripe", accountID); err != nil {
			t.Fatalf("enable first version: %v", err)
		}
		olderVersionID := uuid.New()
		if err := s.EnableWorkspaceServiceVersion(ctx, svcID, "2026-07-08", olderVersionID, accountID); err != nil {
			t.Fatalf("enable older version: %v", err)
		}
		if err := s.EnableWorkspaceServiceVersion(ctx, svcID, "2026-07-08", olderVersionID, accountID); err != nil {
			t.Fatalf("enable older version idempotently: %v", err)
		}

		versions, err := s.ListWorkspaceServiceVersions(ctx, svcID)
		if err != nil {
			t.Fatalf("ListWorkspaceServiceVersions: %v", err)
		}
		if got, want := len(versions), 2; got != want {
			t.Fatalf("expected %d enabled versions, got %d: %#v", want, got, versions)
		}
		seen := map[string]bool{}
		for _, v := range versions {
			seen[v.Version] = true
		}
		for _, want := range []string{"2026-07-09", "2026-07-08"} {
			if !seen[want] {
				t.Errorf("expected enabled version %q, got %#v", want, versions)
			}
		}

		latestVersion, err := s.GetLatestWorkspaceServiceVersionByWorkspace(ctx, svcID)
		if err != nil {
			t.Fatalf("GetLatestWorkspaceServiceVersionByWorkspace: %v", err)
		}
		if latestVersion != "2026-07-08" {
			t.Errorf("expected latest enabled version to become %q, got %q", "2026-07-08", latestVersion)
		}
	})

	t.Run("TestListWorkspaceServiceVersionsForServices_GroupsByService", func(t *testing.T) {
		svcA := uuid.New()
		svcB := uuid.New()
		svcC := uuid.New() // never activated -- must be absent from the result, not a zero-value entry

		if err := s.AddWorkspaceServiceVersion(ctx, svcA, "", "2026-07-01", uuid.New(), "Okta", accountID); err != nil {
			t.Fatalf("activate svcA: %v", err)
		}
		if err := s.EnableWorkspaceServiceVersion(ctx, svcA, "2026-08-01", uuid.New(), accountID); err != nil {
			t.Fatalf("enable svcA extra version: %v", err)
		}
		if err := s.AddWorkspaceServiceVersion(ctx, svcB, "", "2026-06-15", uuid.New(), "GitHub", accountID); err != nil {
			t.Fatalf("activate svcB: %v", err)
		}

		grouped, err := s.ListWorkspaceServiceVersionsForServices(ctx, []uuid.UUID{svcA, svcB, svcC})
		if err != nil {
			t.Fatalf("ListWorkspaceServiceVersionsForServices: %v", err)
		}
		if got, want := len(grouped[svcA]), 2; got != want {
			t.Errorf("expected %d versions for svcA, got %d: %#v", want, got, grouped[svcA])
		}
		if got, want := len(grouped[svcB]), 1; got != want {
			t.Errorf("expected %d version for svcB, got %d: %#v", want, got, grouped[svcB])
		}
		if _, ok := grouped[svcC]; ok {
			t.Errorf("expected no entry for an unactivated service, got %#v", grouped[svcC])
		}

		// The single-service method must agree with the batched one now that
		// it delegates to it -- same data, same grouping, just unwrapped.
		single, err := s.ListWorkspaceServiceVersions(ctx, svcA)
		if err != nil {
			t.Fatalf("ListWorkspaceServiceVersions: %v", err)
		}
		if len(single) != len(grouped[svcA]) {
			t.Errorf("ListWorkspaceServiceVersions and the batched form disagree: %d vs %d", len(single), len(grouped[svcA]))
		}
	})

	t.Run("TestBootstrapWorkspace_RejectsDifferentAccount", func(t *testing.T) {
		_, err := s.BootstrapWorkspace(ctx, uuid.New(), "Other Workspace")
		if !errors.Is(err, ErrWorkspaceOwnerMismatch) {
			t.Fatalf("expected ErrWorkspaceOwnerMismatch, got %v", err)
		}
	})

	t.Run("TestRemoveWorkspaceService_RemovesRow", func(t *testing.T) {
		svcID := uuid.New()

		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "2.0", uuid.Nil, "GitHub", accountID); err != nil {
			t.Fatalf("activate: %v", err)
		}

		if err := s.RemoveWorkspaceService(ctx, svcID); err != nil {
			t.Fatalf("RemoveWorkspaceService: %v", err)
		}

		// Row must be gone — a second deactivate must return ErrWorkspaceServiceNotFound.
		if err := s.RemoveWorkspaceService(ctx, svcID); err == nil {
			t.Error("expected ErrWorkspaceServiceNotFound on second deactivate, got nil")
		}

		// And the service must not appear in the list.
		list, err := s.ListWorkspaceServices(ctx, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceServices after deactivate: %v", err)
		}
		for _, a := range list {
			if a.ServiceID == svcID {
				t.Errorf("ListWorkspaceServices still shows deactivated service %s", svcID)
			}
		}
	})

	// TestIsWorkspaceServiceEnabled_* back Task 2 of
	// engine_workspace_registration_plan.md: a targeted single-row existence
	// check, not a full ListWorkspaceServices fetch, used by the auto-register
	// intercept (Task 3) and the /sdks/generate workspace gate (Task 6).
	t.Run("TestIsWorkspaceServiceEnabled_TrueWhenActivated", func(t *testing.T) {
		svcID := uuid.New()
		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "1.0", uuid.Nil, "Stripe", accountID); err != nil {
			t.Fatalf("activate: %v", err)
		}

		activated, err := s.IsWorkspaceServiceEnabled(ctx, svcID)
		if err != nil {
			t.Fatalf("IsWorkspaceServiceEnabled: %v", err)
		}
		if !activated {
			t.Error("expected true for a service that was just activated")
		}
	})

	t.Run("TestIsWorkspaceServiceEnabled_FalseWhenNeverActivated", func(t *testing.T) {
		svcID := uuid.New()

		activated, err := s.IsWorkspaceServiceEnabled(ctx, svcID)
		if err != nil {
			t.Fatalf("IsWorkspaceServiceEnabled: %v", err)
		}
		if activated {
			t.Error("expected false for a service never activated in this workspace")
		}
	})

	t.Run("TestIsWorkspaceServiceEnabled_FalseAfterDeactivation", func(t *testing.T) {
		svcID := uuid.New()
		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "1.0", uuid.Nil, "GitHub", accountID); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if err := s.RemoveWorkspaceService(ctx, svcID); err != nil {
			t.Fatalf("RemoveWorkspaceService: %v", err)
		}

		activated, err := s.IsWorkspaceServiceEnabled(ctx, svcID)
		if err != nil {
			t.Fatalf("IsWorkspaceServiceEnabled: %v", err)
		}
		if activated {
			t.Error("expected false after deactivation")
		}
	})

	t.Run("TestIsWorkspaceServiceVersionActive_UsesExactTuple", func(t *testing.T) {
		svcID := uuid.New()
		versionID := uuid.New()
		if err := s.AddWorkspaceServiceVersion(ctx, svcID, "", "2026-07-23", versionID, "Jira", accountID); err != nil {
			t.Fatalf("activate exact version: %v", err)
		}

		active, err := s.(WorkspaceServiceVersionStatusStore).IsWorkspaceServiceVersionActive(ctx, svcID, versionID)
		if err != nil || !active {
			t.Fatalf("exact active version lookup: active=%v err=%v", active, err)
		}
		active, err = s.(WorkspaceServiceVersionStatusStore).IsWorkspaceServiceVersionActive(ctx, svcID, uuid.New())
		if err != nil || active {
			t.Fatalf("unknown version lookup: active=%v err=%v", active, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE fused_workspace_service_versions SET status = 'deprecated' WHERE service_id = $1 AND service_version_id = $2`, svcID, versionID); err != nil {
			t.Fatalf("deprecate exact version: %v", err)
		}
		active, err = s.(WorkspaceServiceVersionStatusStore).IsWorkspaceServiceVersionActive(ctx, svcID, versionID)
		if err != nil || active {
			t.Fatalf("deprecated version lookup: active=%v err=%v", active, err)
		}
	})

	t.Run("TestListWorkspaceServicesPage", func(t *testing.T) {

		svcID1 := uuid.New()
		svcID2 := uuid.New()
		svcID3 := uuid.New()
		accountID := uuid.New()

		if err := s.AddWorkspaceServiceVersion(ctx, svcID1, "", "1.0", uuid.New(), "Service One", accountID); err != nil {
			t.Fatalf("activate svcID1: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		if err := s.AddWorkspaceServiceVersion(ctx, svcID2, "", "2.0", uuid.New(), "Service Two", accountID); err != nil {
			t.Fatalf("activate svcID2: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		if err := s.AddWorkspaceServiceVersion(ctx, svcID3, "", "3.0", uuid.New(), "Service Three", accountID); err != nil {
			t.Fatalf("third AddWorkspaceServiceVersion: %v", err)
		}

		// Pagination test: Limit 2, Offset 0.
		// Ordering is created_at DESC. Expected order: svc3, svc2, svc1
		services, total, err := s.ListWorkspaceServicesPage(ctx, nil, 2, 0)
		if err != nil {
			t.Fatalf("ListWorkspaceServicesPage (0-1): %v", err)
		}
		// total might include services added by earlier subtests. We just verify the relative logic.
		if len(services) != 2 {
			t.Fatalf("Expected 2 services, got %d", len(services))
		}
		if services[0].ServiceID != svcID3 {
			t.Errorf("Expected first service to be svc3, got %s", services[0].ServiceID)
		}
		if services[1].ServiceID != svcID2 {
			t.Errorf("Expected second service to be svc2, got %s", services[1].ServiceID)
		}
		if total < 3 {
			t.Errorf("Expected total to be at least 3, got %d", total)
		}

		// Pagination test: Limit 2, Offset 2
		services2, _, err := s.ListWorkspaceServicesPage(ctx, nil, 2, 2)
		if err != nil {
			t.Fatalf("ListWorkspaceServicesPage (2-3): %v", err)
		}
		if len(services2) == 0 {
			t.Fatalf("Expected at least 1 service, got 0")
		}
		if services2[0].ServiceID != svcID1 {
			t.Errorf("Expected first service in page 2 to be svc1, got %s", services2[0].ServiceID)
		}

		// Filter test
		filtered, filterTotal, err := s.ListWorkspaceServicesPage(ctx, []string{"Service Two"}, 10, 0)
		if err != nil {
			t.Fatalf("ListWorkspaceServicesPage filtered: %v", err)
		}
		if filterTotal != 1 {
			t.Errorf("Expected filtered total 1, got %d", filterTotal)
		}
		if len(filtered) != 1 || filtered[0].ServiceID != svcID2 {
			t.Errorf("Expected filtered list to contain only svc2")
		}
	})
}
