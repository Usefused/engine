package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestDeleteBucketHandlerAllowsConnectedUsersAfterUIConfirmation(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	s := &bucketDeleteGuardStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		bucket:      store.Bucket{ID: bucketID, Name: "prod"},
		summary:     store.BucketConnectSummary{BucketID: bucketID, ConnectedUserCount: 2},
	}
	rr := deleteBucketForTest(t, s, "prod")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
	if !s.deleted {
		t.Fatal("delete should run after the UI has collected explicit confirmation")
	}
}

func TestDeleteBucketHandlerAllowsUnusedBucket(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	s := &bucketDeleteGuardStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		bucket:      store.Bucket{ID: bucketID, Name: "prod"},
		summary:     store.BucketConnectSummary{BucketID: bucketID},
	}
	rr := deleteBucketForTest(t, s, "prod")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
	if !s.deleted {
		t.Fatal("delete should run for unused non-default bucket")
	}
}

type bucketDeleteGuardStore struct {
	store.Store
	accountID   uuid.UUID
	workspaceID uuid.UUID
	bucket      store.Bucket
	summary     store.BucketConnectSummary
	deleted     bool
}

func (s *bucketDeleteGuardStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	return s.accountID, nil
}

func (s *bucketDeleteGuardStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

func (s *bucketDeleteGuardStore) GetBucketByName(context.Context, string) (*store.Bucket, error) {
	return &s.bucket, nil
}

func (s *bucketDeleteGuardStore) GetBucketConnectSummary(context.Context, uuid.UUID) (*store.BucketConnectSummary, error) {
	return &s.summary, nil
}

func (s *bucketDeleteGuardStore) DeleteBucket(context.Context, string) error {
	s.deleted = true
	return nil
}

func deleteBucketForTest(t *testing.T, s store.Store, name string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Delete("/workspace/buckets/{name}", DeleteBucketHandler(s))
	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+name, nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}
