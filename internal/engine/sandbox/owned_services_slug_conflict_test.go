package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type ownedServiceActivationFailureStore struct {
	*ownedServiceWorkspaceStub
	failures map[uuid.UUID]error
}

// AddWorkspaceServiceVersion models an atomic failed activation without changing the pre-existing membership.
func (s *ownedServiceActivationFailureStore) AddWorkspaceServiceVersion(ctx context.Context, id uuid.UUID, slug, version string, versionID uuid.UUID, name string, owner uuid.UUID) error {
	// Only explicitly injected services fail; other admitted peers use normal activation behavior.
	if err := s.failures[id]; err != nil {
		return err
	}
	return s.ownedServiceWorkspaceStub.AddWorkspaceServiceVersion(ctx, id, slug, version, versionID, name, owner)
}

// TestOwnedServiceRecoveryDefersSlugConflict verifies healthy peers recover and a restart retains every existing pin.
func TestOwnedServiceRecoveryDefersSlugConflict(t *testing.T) {
	existing, conflicting, good := recoveryTestService(), recoveryTestService(), recoveryTestService()
	registry := &ownedServiceRegistryStub{services: []OwnedRegistryService{existing, conflicting, good}}
	workspace := &ownedServiceActivationFailureStore{
		ownedServiceWorkspaceStub: &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{existing.ServiceID: true}},
		failures:                  map[uuid.UUID]error{conflicting.ServiceID: fmt.Errorf("activate: %w", store.ErrWorkspaceServiceSlugConflict)},
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := ReconcileOwnedServices(context.Background(), workspace, registry, uuid.New(), "license")
		// The conflicting service remains visible as deferred instead of making every startup fatal.
		if err != nil || len(result.Deferred) != 1 || result.Deferred[0].ServiceID != conflicting.ServiceID || result.Deferred[0].ServiceVersionID != conflicting.ServiceVersionID || !errors.Is(result.Deferred[0].cause, store.ErrWorkspaceServiceSlugConflict) {
			t.Fatalf("attempt %d: result=%+v err=%v", attempt, result, err)
		}
		// The first pass activates the healthy peer; the next pass must recognize its existing pin.
		if result.Discovered != 3 || result.Activated != 1-attempt || result.AlreadyActive != 1+attempt {
			t.Fatalf("attempt %d: incorrect recovery counts %+v", attempt, result)
		}
	}
	// Neither the original service nor the conflict may be republished by recovery.
	if len(workspace.added) != 1 || workspace.added[0].ServiceID != good.ServiceID || !workspace.active[existing.ServiceID] || workspace.active[conflicting.ServiceID] {
		t.Fatalf("unexpected activations: %+v", workspace.added)
	}
}

// TestOwnedServiceRecoveryActivationFailureRemainsFatal prevents the conflict exception from swallowing infrastructure errors.
func TestOwnedServiceRecoveryActivationFailureRemainsFatal(t *testing.T) {
	for _, failure := range []error{
		errors.New("database unavailable"),
		&pgconn.PgError{Code: "23505", ConstraintName: "unrelated_unique_constraint"},
	} {
		service := recoveryTestService()
		registry := &ownedServiceRegistryStub{services: []OwnedRegistryService{service}}
		workspace := &ownedServiceActivationFailureStore{
			ownedServiceWorkspaceStub: &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{}},
			failures:                  map[uuid.UUID]error{service.ServiceID: failure},
		}
		result, err := ReconcileOwnedServices(context.Background(), workspace, registry, uuid.New(), "license")
		// Unknown activation failures retain their original cause and cannot report a successful partial recovery.
		if !errors.Is(err, failure) || result.Activated != 0 || len(result.Deferred) != 0 {
			t.Fatalf("result=%+v err=%v; want fatal %v", result, err, failure)
		}
	}
}
