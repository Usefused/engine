package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type workspaceSlugErrorWriter struct{ err error }

// Exec injects database errors at the production upsert boundary without requiring PostgreSQL.
func (w workspaceSlugErrorWriter) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, w.err
}

// TestWorkspaceSlugConflictClassification prevents unrelated constraints or error prose from becoming recoverable.
func TestWorkspaceSlugConflictClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		conflict bool
	}{
		{"slug index", &pgconn.PgError{Code: "23505", ConstraintName: "uq_fused_workspace_services_slug_ci"}, true},
		{"different unique index", &pgconn.PgError{Code: "23505", ConstraintName: "another_index"}, false},
		{"different SQL state", &pgconn.PgError{Code: "40001", ConstraintName: "uq_fused_workspace_services_slug_ci"}, false},
		{"untrusted prose", errors.New("duplicate key uq_fused_workspace_services_slug_ci SQLSTATE 23505"), false},
		{"success", nil, false},
	} {
		// Every case exercises classification through the real service upsert, including the nil-error path.
		t.Run(tc.name, func(t *testing.T) {
			err := upsertWorkspaceService(context.Background(), workspaceSlugErrorWriter{tc.err}, uuid.New(), "shared", "Shared", uuid.New())
			// Recovery must require both the exact database code and the exact slug constraint.
			if errors.Is(err, ErrWorkspaceServiceSlugConflict) != tc.conflict || !errors.Is(err, tc.err) {
				t.Fatalf("classified error = %v; want conflict=%v with original cause", err, tc.conflict)
			}
		})
	}
}

// TestWorkspaceSlugConflictPreservesExistingPins exercises the real index and activation transaction rollback.
func TestWorkspaceSlugConflictPreservesExistingPins(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	owner, existing, conflicting := uuid.New(), uuid.New(), uuid.New()
	existingVersion, conflictingVersion := uuid.New(), uuid.New()
	// The current slug owner must exist before the new service attempts to claim a case variant.
	if err := repository.AddWorkspaceServiceVersion(ctx, existing, "shared", "v1", existingVersion, "Existing", owner); err != nil {
		t.Fatal(err)
	}
	err := repository.AddWorkspaceServiceVersion(ctx, conflicting, "SHARED", "v2", conflictingVersion, "Incoming", owner)
	var postgresError *pgconn.PgError
	// The typed error must retain SQL diagnostics while identifying only this recoverable conflict.
	if !errors.Is(err, ErrWorkspaceServiceSlugConflict) || !errors.As(err, &postgresError) || postgresError.Code != "23505" {
		t.Fatalf("expected typed slug collision, got %v", err)
	}
	services, err := repository.ListWorkspaceServices(ctx, nil)
	// Rollback must preserve the existing display metadata and exact version without activating the incoming service.
	if err != nil || len(services) != 1 || services[0].ServiceID != existing || services[0].ServiceVersionID != existingVersion || services[0].ServiceSlug != "shared" || services[0].ServiceName != "Existing" {
		t.Fatalf("existing service changed: services=%+v err=%v", services, err)
	}
	var incomingVersions int
	// No orphan enabled-version row may remain after a rejected membership write.
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM fused_workspace_service_versions WHERE service_id = $1", conflicting).Scan(&incomingVersions); err != nil || incomingVersions != 0 {
		t.Fatalf("conflicting versions=%d err=%v", incomingVersions, err)
	}
	// An explicit distinct slug resolves the conflict without replacing the original service or its pin.
	if err := repository.AddWorkspaceServiceVersion(ctx, conflicting, "distinct", "v2", conflictingVersion, "Incoming", owner); err != nil {
		t.Fatalf("activation after slug repair: %v", err)
	}
}
