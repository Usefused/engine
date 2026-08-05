package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
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
func (*artifactSnapshotStoreStub) GetArtifactSnapshotByName(context.Context, uuid.UUID, string, string) (*store.ArtifactSnapshot, error) {
	return nil, store.ErrArtifactSnapshotNotFound
}
func (*artifactSnapshotStoreStub) GetArtifactSnapshotByIdentity(context.Context, uuid.UUID, string, string, string) (*store.ArtifactSnapshot, error) {
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

func TestFetchOwnedArtifactSnapshotsUsesCurrentPortableSelectionContract(t *testing.T) {
	accountID, artifactID, serviceID, versionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{endpoint: "https://registry.example/graphql", licenseKey: "engine-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"sdks":{"total":1,"items":[{"id":"` + artifactID.String() + `","name":"jira","version":"1.0.0","target_type":"sdk","target_language":"typescript","created_at":"2026-08-05T00:00:00Z","detailed_selections":[{"service_id":"` + serviceID.String() + `","service_version_id":"` + versionID.String() + `","endpoint_ids":["` + uuid.New().String() + `"],"operation_names":["getIssue"],"auth_type":"oauth2","auth_name":"jiraOAuth","connect_scopes":["read:jira-work"],"injections":[{"location":"header","name":"X-Tenant","value":"$connection.tenant","mode":"replace"}]}] }]}}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}}

	snapshots, err := client.FetchOwnedArtifactSnapshots(context.Background(), accountID)
	if err != nil {
		t.Fatalf("FetchOwnedArtifactSnapshots = %#v, %v", snapshots, err)
	}
	assertOwnedArtifactCount(t, len(snapshots), 1)
	if !strings.Contains(requestBody.Query, "injections { location name value mode }") {
		t.Fatalf("portable query omitted injections: %s", requestBody.Query)
	}
	var selections []models.SDKSelection
	if err := json.Unmarshal(snapshots[0].Selections, &selections); err != nil {
		t.Fatalf("decode snapshot selections: %#v, %v", selections, err)
	}
	assertOwnedArtifactCount(t, len(selections), 1)
	selection := selections[0]
	if selection.AuthName != "jiraOAuth" {
		t.Fatalf("portable selection metadata was lost: %+v", selection)
	}
	assertOwnedArtifactCount(t, len(selection.OperationNames), 1)
	assertOwnedArtifactCount(t, len(selection.Injections), 1)
	if selection.Injections[0].Mode != "replace" {
		t.Fatalf("portable injection mode was lost: %+v", selection.Injections[0])
	}
}

func TestFetchOwnedArtifactSnapshotsRejectsRegistryContractMismatch(t *testing.T) {
	requests := 0
	client := &HTTPRegistryClient{endpoint: "https://registry.example/graphql", licenseKey: "engine-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			body := `{"errors":[{"message":"Cannot query field \"auth_type\" on type \"SDKSelectionDetail\"."}]}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}}

	_, err := client.FetchOwnedArtifactSnapshots(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "Registry contract mismatch") {
		t.Fatalf("expected explicit contract mismatch, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("contract mismatch issued %d requests, want one current-contract request", requests)
	}
}

func assertOwnedArtifactCount(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("item count = %d, want %d", got, want)
	}
}
