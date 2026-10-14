package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type identityRefreshTestStore struct {
	*workspaceTestStore
	serviceID, versionID uuid.UUID
	reference, name      string
	refreshErr           error
}

// RefreshWorkspaceServiceIdentity records verified metadata separately from activation and snapshot writes.
func (s *identityRefreshTestStore) RefreshWorkspaceServiceIdentity(_ context.Context, serviceID, versionID uuid.UUID, reference, name string) error {
	s.serviceID, s.versionID, s.reference, s.name = serviceID, versionID, reference, name
	return s.refreshErr
}

// TestActiveServiceRefreshesVerifiedIdentity prevents the idempotent activation path from leaving stale references behind.
func TestActiveServiceRefreshesVerifiedIdentity(t *testing.T) {
	for _, refreshErr := range []error{nil, store.ErrWorkspaceServiceSlugConflict} {
		serviceID, versionID := uuid.New(), uuid.New()
		s := &identityRefreshTestStore{
			workspaceTestStore: &workspaceTestStore{workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
				serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1", Status: "active"}},
			}},
			refreshErr: refreshErr,
		}
		verifier := &mockVerifier{name: "OpenAI", serviceVersionID: versionID, currentVersionTag: "v1"}
		err := verifyAndActivateService(context.Background(), store.NewCachedStore(s, nil), verifier, addServiceCall{
			accountID: uuid.New(), serviceID: serviceID, versionID: versionID, version: "v1",
		})
		// The cache wrapper must forward exact verified identity and retain failure classification.
		if !errors.Is(err, refreshErr) || s.serviceID != serviceID || s.versionID != versionID || s.reference != "@test/"+serviceID.String() || s.name != "OpenAI" {
			t.Fatalf("refresh=%+v err=%v want=%v", s, err, refreshErr)
		}
		// Neither successful refresh nor a slug collision may rewrite membership or the admitted snapshot.
		if s.activationCalls.Load() != 0 || len(s.snapshotWrites) != 0 {
			t.Fatalf("activation=%d snapshots=%d", s.activationCalls.Load(), len(s.snapshotWrites))
		}
	}
}

// TestWorkspaceServiceProjectionPreservesResolvableReference exposes stale metadata instead of masking it with Registry enrichment.
func TestWorkspaceServiceProjectionPreservesResolvableReference(t *testing.T) {
	id := uuid.New()
	for _, localReference := range []string{"openai", "@martins-joseph/openai"} {
		for _, metadata := range []map[uuid.UUID]sandbox.ServiceVisibility{
			nil,
			{id: {Slug: "openai", CanonicalRef: "@martins-joseph/openai", IsOwner: true}},
		} {
			rows := projectGraphQLWorkspaceServices([]store.WorkspaceService{{ServiceID: id, ServiceSlug: localReference}}, nil, metadata, nil)
			// Listing and exact local filtering must agree regardless of Registry availability or ownership.
			if len(rows) != 1 || rows[0]["service_slug"] != localReference {
				t.Fatalf("local=%s rows=%v", localReference, rows)
			}
			// The Registry's canonical identity remains separately available for reconciliation and display.
			if metadata != nil && rows[0]["canonical_ref"] != "@martins-joseph/openai" {
				t.Fatalf("Registry identity lost: %v", rows)
			}
		}
	}
}
