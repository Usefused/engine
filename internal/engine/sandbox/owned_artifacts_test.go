package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type ownedArtifactRegistryStub struct {
	snapshots []store.ArtifactSnapshot
	err       error
}

func (s ownedArtifactRegistryStub) FetchOwnedArtifactSnapshots(context.Context, uuid.UUID) ([]store.ArtifactSnapshot, error) {
	return s.snapshots, s.err
}

type artifactSnapshotStoreStub struct{ saved []store.ArtifactSnapshot }

func (s *artifactSnapshotStoreStub) UpsertArtifactSnapshots(_ context.Context, snapshots []store.ArtifactSnapshot) error {
	s.saved = append(s.saved, snapshots...)
	return nil
}
func (*artifactSnapshotStoreStub) DeleteArtifactSnapshot(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (*artifactSnapshotStoreStub) GetArtifactSnapshot(context.Context, uuid.UUID, uuid.UUID) (*store.ArtifactSnapshot, error) {
	return nil, store.ErrArtifactSnapshotNotFound
}
func (*artifactSnapshotStoreStub) ListArtifactSnapshots(context.Context, uuid.UUID, string, int, int) ([]store.ArtifactSnapshot, int, error) {
	return nil, 0, nil
}

func TestReconcileOwnedArtifactsPersistsOneBatchWithoutRuntimeCredentials(t *testing.T) {
	accountID := uuid.New()
	snapshots := []store.ArtifactSnapshot{{ArtifactID: uuid.New(), AccountID: accountID, Kind: "sdk", Name: "support", Selections: []byte("[]")}}
	destination := &artifactSnapshotStoreStub{}
	count, err := ReconcileOwnedArtifacts(context.Background(), destination, ownedArtifactRegistryStub{snapshots: snapshots}, accountID)
	if err != nil || count != 1 || len(destination.saved) != 1 {
		t.Fatalf("reconcile = count %d saved %d err %v", count, len(destination.saved), err)
	}
	if destination.saved[0].Active {
		t.Fatal("Registry restore must not fabricate an executable credential scope")
	}
}

func TestReconcileOwnedArtifactsDoesNotWriteAfterRegistryFailure(t *testing.T) {
	destination := &artifactSnapshotStoreStub{}
	_, err := ReconcileOwnedArtifacts(context.Background(), destination, ownedArtifactRegistryStub{err: errors.New("offline")}, uuid.New())
	if err == nil || len(destination.saved) != 0 {
		t.Fatalf("expected fetch failure without writes, saved=%d err=%v", len(destination.saved), err)
	}
}
