package api

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type providerReferenceVerifier struct {
	ServiceVerifier
	refs map[uuid.UUID]string
}

// VerifyServiceExists models Registry-authoritative identity independently of the submitted YAML key.
func (v providerReferenceVerifier) VerifyServiceExists(_ context.Context, id uuid.UUID, _ string) (string, string, string, uuid.UUID, error) {
	return "Plunk", v.refs[id], "v1", uuid.New(), nil
}

type providerReferenceStore struct {
	store.Store
	refs map[uuid.UUID]string
}

// AddWorkspaceServiceVersion records the reference passed to the persistence boundary.
func (s providerReferenceStore) AddWorkspaceServiceVersion(_ context.Context, id uuid.UUID, ref, _ string, _ uuid.UUID, _ string, _ uuid.UUID) error {
	s.refs[id] = ref
	return nil
}

// TestWorkspaceApplyPreservesVerifiedProviderReferences covers online apply and its network-free transaction phase.
func TestWorkspaceApplyPreservesVerifiedProviderReferences(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	refs := map[uuid.UUID]string{a: "@account-a/plunk", b: "@account-b/plunk"}
	verifier := providerReferenceVerifier{refs: refs}
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{
		a: {ServiceID: a, Key: a.String(), Versions: []string{"v1"}, VersionIDs: map[string]uuid.UUID{"v1": uuid.New()}},
		b: {ServiceID: b, Key: "@untrusted/plunk", Versions: []string{"v1"}, VersionIDs: map[string]uuid.UUID{"v1": uuid.New()}},
	}}
	for _, online := range []bool{true, false} {
		var activeVerifier ServiceVerifier = verifier
		// Partial apply preflights identity before entering its transaction; it must not revert to the authored key inside it.
		if !online {
			activeVerifier = nil
			for id, service := range desired.Services {
				name, reference, err := verifiedWorkspaceServiceMetadata(context.Background(), verifier, service, "license")
				// Failed verification cannot supply transaction-local identity.
				if err != nil {
					t.Fatal(err)
				}
				service.ServiceName, service.ServiceReference = name, reference
				desired.Services[id] = service
			}
		}
		workspace := providerReferenceStore{refs: map[uuid.UUID]string{}}
		_, err := upsertDesiredWorkspaceServices(context.Background(), workspace, activeVerifier, "license", uuid.New(), desired)
		// Both paths must persist separate Registry references even when YAML uses an opaque ID or a misleading alias.
		if err != nil || workspace.refs[a] != refs[a] || workspace.refs[b] != refs[b] {
			t.Fatalf("online=%v refs=%v err=%v", online, workspace.refs, err)
		}
	}
}
