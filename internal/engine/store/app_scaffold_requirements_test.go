package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type appScaffoldSelectionTestRow struct {
	index            int
	serviceKey       string
	matchCount       int
	serviceID        *uuid.UUID
	serviceVersionID *uuid.UUID
}

type appScaffoldSelectionTestRows struct {
	rows   []appScaffoldSelectionTestRow
	cursor int
	err    error
}

// Next advances the bounded in-memory row fixture used to isolate cardinality behavior.
func (r *appScaffoldSelectionTestRows) Next() bool {
	// The cursor cannot advance past the declared fake result set.
	if r.cursor >= len(r.rows) {
		return false
	}
	r.cursor++
	return true
}

// Scan projects one fake PostgreSQL row into the collector's typed destinations.
func (r *appScaffoldSelectionTestRows) Scan(destinations ...any) error {
	row := r.rows[r.cursor-1]
	*destinations[0].(*int) = row.index
	*destinations[1].(*string) = row.serviceKey
	*destinations[2].(*int) = row.matchCount
	*destinations[3].(**uuid.UUID) = row.serviceID
	*destinations[4].(**uuid.UUID) = row.serviceVersionID
	return nil
}

// Err exposes a deferred stream error after the fake rows are exhausted.
func (r *appScaffoldSelectionTestRows) Err() error {
	return r.err
}

// TestResolveAuthorizedAppScaffoldSelectionsUsesOneSetQuery protects the
// authorization, exact-version, and active-row predicates in the one SQL read.
func TestResolveAuthorizedAppScaffoldSelectionsUsesOneSetQuery(t *testing.T) {
	fragments := []string{
		"unnest($1::integer[], $2::text[], $3::text[])",
		"versions.version = requested.version",
		"versions.status <> 'deprecated'",
		"$4 OR services.service_id = ANY($5::uuid[])",
		"COUNT(candidate.service_id)",
	}
	// Every predicate belongs in the same statement so no per-service lookup can appear.
	for _, fragment := range fragments {
		if !strings.Contains(resolveAuthorizedAppScaffoldSelectionsSQL, fragment) {
			t.Fatalf("resolution SQL missing %q: %s", fragment, resolveAuthorizedAppScaffoldSelectionsSQL)
		}
	}
	// Exactly one set-input expression maps the three aligned arrays.
	if strings.Count(resolveAuthorizedAppScaffoldSelectionsSQL, "unnest(") != 1 {
		t.Fatalf("resolution SQL must use one set input: %s", resolveAuthorizedAppScaffoldSelectionsSQL)
	}
}

// TestCollectAppScaffoldResolvedSelectionsEnforcesCardinality verifies absent,
// ambiguous, and truncated batches all fail without exposing which label missed.
func TestCollectAppScaffoldResolvedSelectionsEnforcesCardinality(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	valid := &appScaffoldSelectionTestRows{rows: []appScaffoldSelectionTestRow{{
		index: 0, serviceKey: "sendbird", matchCount: 1, serviceID: &serviceID, serviceVersionID: &versionID,
	}}}
	resolved, err := collectAppScaffoldResolvedSelections(valid, 1)
	// A unique match retains the immutable service/version identity.
	if err != nil || len(resolved) != 1 || resolved[0].ServiceID != serviceID || resolved[0].ServiceVersionID != versionID {
		t.Fatalf("resolved = %#v, err %v", resolved, err)
	}

	cases := map[string]*appScaffoldSelectionTestRows{
		"missing":   {rows: []appScaffoldSelectionTestRow{{index: 0, serviceKey: "sendbird", matchCount: 0}}},
		"ambiguous": {rows: []appScaffoldSelectionTestRow{{index: 0, serviceKey: "sendbird", matchCount: 2}}},
		"truncated": {rows: []appScaffoldSelectionTestRow{{index: 0, serviceKey: "sendbird", matchCount: 1, serviceID: &serviceID, serviceVersionID: &versionID}}},
	}
	// Each invalid shape must collapse to the same unavailable sentinel.
	for name, rows := range cases {
		expected := 1
		// The truncated fixture represents a two-selection request with one returned row.
		if name == "truncated" {
			expected = 2
		}
		_, err := collectAppScaffoldResolvedSelections(rows, expected)
		// Error identity remains stable even when the wrapped selection index differs.
		if !errors.Is(err, ErrAppScaffoldSelectionUnavailable) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

// TestAppScaffoldSelectionArraysPreserveMapping guards the parallel-array join
// against service/version drift when input indexes are non-sequential.
func TestAppScaffoldSelectionArraysPreserveMapping(t *testing.T) {
	indexes, keys, versions := appScaffoldSelectionArrays([]AppScaffoldSelectionRef{
		{SelectionIndex: 4, ServiceKey: "sendbird", Version: "v2"},
		{SelectionIndex: 1, ServiceKey: "stripe", Version: "v1"},
	})
	// Positional equality is the contract consumed by PostgreSQL unnest.
	if len(indexes) != 2 || indexes[0] != 4 || keys[0] != "sendbird" || versions[0] != "v2" || indexes[1] != 1 || keys[1] != "stripe" || versions[1] != "v1" {
		t.Fatalf("arrays = %#v/%#v/%#v", indexes, keys, versions)
	}
}

// TestPostgresStoreResolvesAuthorizedAppScaffoldSelections exercises exact
// active-version and service.read filtering against the Engine schema.
func TestPostgresStoreResolvesAuthorizedAppScaffoldSelections(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Integration coverage is opt-in so ordinary unit runs need no PostgreSQL process.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Schema/bootstrap failures should surface rather than silently downgrading coverage.
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)
	allowedService, deniedService := uuid.New(), uuid.New()
	allowedVersion, deniedVersion := uuid.New(), uuid.New()
	allowedSlug, deniedSlug := "sendbird-scaffold-"+allowedService.String(), "denied-scaffold-"+deniedService.String()
	enabledBy := uuid.New()
	defer cleanupAppScaffoldServices(t, ctx, pool, allowedService, deniedService)
	// Two active services prove the SQL scope filters before exact version resolution.
	if err := repository.AddWorkspaceServiceVersion(ctx, allowedService, allowedSlug, "v1", allowedVersion, "Sendbird Scaffold Test", enabledBy); err != nil {
		t.Fatalf("add allowed service: %v", err)
	}
	if err := repository.AddWorkspaceServiceVersion(ctx, deniedService, deniedSlug, "v1", deniedVersion, "Denied Scaffold Test", enabledBy); err != nil {
		t.Fatalf("add denied service: %v", err)
	}
	scope := accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedService}}
	resolved, err := repository.ResolveAuthorizedAppScaffoldSelections(ctx, scope, []AppScaffoldSelectionRef{{
		SelectionIndex: 0, ServiceKey: "@provider/" + allowedSlug, Version: "v1",
	}})
	// Provider-qualified keys resolve to the exact authorized immutable version.
	if err != nil || len(resolved) != 1 || resolved[0].ServiceID != allowedService || resolved[0].ServiceVersionID != allowedVersion {
		t.Fatalf("resolved = %#v, err %v", resolved, err)
	}
	_, err = repository.ResolveAuthorizedAppScaffoldSelections(ctx, scope, []AppScaffoldSelectionRef{{
		SelectionIndex: 0, ServiceKey: deniedSlug, Version: "v1",
	}})
	// A real but unauthorized service is indistinguishable from an unavailable one.
	if !errors.Is(err, ErrAppScaffoldSelectionUnavailable) {
		t.Fatalf("denied resolution error = %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE fused_workspace_service_versions SET status = 'deprecated' WHERE service_id = $1 AND service_version_id = $2`, allowedService, allowedVersion)
	// Fixture mutation failure would invalidate the active-version assertion.
	if err != nil {
		t.Fatalf("deprecate allowed version: %v", err)
	}
	_, err = repository.ResolveAuthorizedAppScaffoldSelections(ctx, scope, []AppScaffoldSelectionRef{{
		SelectionIndex: 0, ServiceKey: allowedSlug, Version: "v1",
	}})
	// Deprecated versions cannot be projected into a new SDK/MCP scaffold.
	if !errors.Is(err, ErrAppScaffoldSelectionUnavailable) {
		t.Fatalf("deprecated resolution error = %v", err)
	}
}

// cleanupAppScaffoldServices removes only UUID-scoped fixtures created by this test.
func cleanupAppScaffoldServices(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, serviceIDs ...uuid.UUID) {
	t.Helper()
	// Cascading deletes clear exact version rows while preserving every unrelated service.
	for _, serviceID := range serviceIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM fused_workspace_services WHERE service_id = $1`, serviceID); err != nil {
			t.Errorf("cleanup scaffold service %s: %v", serviceID, err)
		}
	}
}
