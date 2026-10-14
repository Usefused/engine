package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestWorkspaceServiceIdentityRefresh preserves immutable pins and ownership while making the verified reference resolvable.
func TestWorkspaceServiceIdentityRefresh(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	serviceID, versionID, owner := uuid.New(), uuid.New(), uuid.New()
	// Reproduce the hosted state: an active version with a legacy bare reference.
	if err := repository.AddWorkspaceServiceVersion(ctx, serviceID, "openai", "v1", versionID, "Old name", owner); err != nil {
		t.Fatal(err)
	}
	before, err := repository.GetWorkspaceServiceVersion(ctx, serviceID, versionID)
	// Preserve every field of the immutable membership pin for comparison after refresh.
	if err != nil {
		t.Fatal(err)
	}
	writer := NewCachedStore(repository, nil).(WorkspaceServiceIdentityWriter)
	for range 2 {
		// Repeated adds repair identity idempotently without changing version timestamps or ownership.
		if err := writer.RefreshWorkspaceServiceIdentity(ctx, serviceID, versionID, "@martins-joseph/openai", "OpenAI"); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := repository.ResolveWorkspaceServiceIDsByKeys(ctx, []string{"@martins-joseph/openai", "@wrong/openai"})
	// Qualified resolution must match the exact provider and reject an unrelated provider namespace.
	if err != nil || len(resolved) != 1 || resolved["@martins-joseph/openai"] != serviceID {
		t.Fatalf("resolved=%v err=%v", resolved, err)
	}
	resolved, err = repository.ResolveGenerationServiceIDsByKeys(ctx, []string{"@martins-joseph/openai", "@wrong/openai"})
	// SDK and Unified App generation must resolve the same repaired identity as workspace listing.
	if err != nil || len(resolved) != 1 || resolved["@martins-joseph/openai"] != serviceID {
		t.Fatalf("generation resolved=%v err=%v", resolved, err)
	}
	after, err := repository.GetWorkspaceServiceVersion(ctx, serviceID, versionID)
	// Metadata repair must not modify activation status, timestamps, version IDs, or the enabling user.
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
	}
	rows, err := repository.ListWorkspaceServices(ctx, []string{"@martins-joseph/openai"})
	// The durable listing must expose the same resolvable reference without changing service ownership.
	if err != nil || len(rows) != 1 || rows[0].ServiceID != serviceID || rows[0].ServiceSlug != "@martins-joseph/openai" || rows[0].ServiceName != "OpenAI" || rows[0].AddedBy != owner {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	// A second service cannot steal the newly repaired namespace, including through case variations.
	otherID, otherVersion := uuid.New(), uuid.New()
	if err := repository.AddWorkspaceServiceVersion(ctx, otherID, "other", "v1", otherVersion, "Other", owner); err != nil {
		t.Fatal(err)
	}
	// Preserve the typed conflict so API callers cannot mistake failed refresh for successful activation.
	if err := writer.RefreshWorkspaceServiceIdentity(ctx, otherID, otherVersion, "@MARTINS-JOSEPH/OPENAI", "Conflicting"); !errors.Is(err, ErrWorkspaceServiceSlugConflict) {
		t.Fatalf("expected slug collision: %v", err)
	}
	rows, err = repository.ListWorkspaceServices(ctx, []string{"other"})
	// The failed statement must roll back all metadata on the competing service.
	if err != nil || len(rows) != 1 || rows[0].ServiceID != otherID || rows[0].ServiceName != "Other" {
		t.Fatalf("collision changed metadata: rows=%v err=%v", rows, err)
	}
	// Simulate deprecation after the caller's active-state check.
	if _, err := pool.Exec(ctx, "UPDATE fused_workspace_service_versions SET status='deprecated' WHERE service_version_id=$1", versionID); err != nil {
		t.Fatal(err)
	}
	// A stale active check cannot authorize metadata changes on a now-deprecated or missing tuple.
	for _, staleVersion := range []uuid.UUID{versionID, uuid.New()} {
		if err := writer.RefreshWorkspaceServiceIdentity(ctx, serviceID, staleVersion, "@wrong/openai", "Wrong"); !errors.Is(err, ErrWorkspaceServiceVersionNotFound) {
			t.Fatalf("expected absent active tuple: %v", err)
		}
	}
}
