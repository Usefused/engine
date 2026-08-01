package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
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
	if s.deletedID != bucketID {
		t.Fatalf("delete authorized bucket = %s, want %s", s.deletedID, bucketID)
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

func TestDeleteBucketHandlerCarriesMiddlewareAuthorizedIDAcrossReplacement(t *testing.T) {
	oldID, replacementID := uuid.New(), uuid.New()
	s := newBucketDeleteGuardStore()
	s.bucket = store.Bucket{ID: replacementID, Name: "prod"}
	s.summary = store.BucketConnectSummary{BucketID: oldID}
	s.authorizedID = oldID

	response := deleteBucketForTest(t, s, "prod")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", response.Code, response.Body.String())
	}
	if s.deletedID != oldID {
		t.Fatalf("delete received replacement ID %s, want authorized ID %s", s.deletedID, oldID)
	}
}

func TestDeleteBucketHandlerMapsDeletionFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		deleteErr  error
		wantStatus int
	}{
		{name: "bound", deleteErr: store.ErrBucketBound, wantStatus: http.StatusConflict},
		{name: "default", deleteErr: store.ErrDefaultBucketProtected, wantStatus: http.StatusConflict},
		{name: "not found", deleteErr: store.ErrBucketNotFound, wantStatus: http.StatusNotFound},
		{name: "repository", deleteErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newBucketDeleteGuardStore()
			s.deleteErr = test.deleteErr
			response := deleteBucketForTest(t, s, "prod")
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if s.deleted {
				t.Fatal("failed deletion must not be recorded as deleted")
			}
		})
	}
}

func TestDeleteBucketHandlerStopsOnInspectionFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		summaryErr error
	}{
		{name: "usage summary", summaryErr: errors.New("summary failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newBucketDeleteGuardStore()
			s.summaryErr = test.summaryErr
			response := deleteBucketForTest(t, s, "prod")
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
			}
			if s.deleted {
				t.Fatal("delete ran after inspection failure")
			}
		})
	}
}

func newBucketDeleteGuardStore() *bucketDeleteGuardStore {
	workspaceID, bucketID := uuid.New(), uuid.New()
	return &bucketDeleteGuardStore{
		accountID: uuid.New(), workspaceID: workspaceID,
		bucket:  store.Bucket{ID: bucketID, Name: "prod"},
		summary: store.BucketConnectSummary{BucketID: bucketID},
	}
}

type bucketDeleteGuardStore struct {
	store.Store
	accountID    uuid.UUID
	workspaceID  uuid.UUID
	bucket       store.Bucket
	summary      store.BucketConnectSummary
	deleted      bool
	deletedID    uuid.UUID
	lookupErr    error
	summaryErr   error
	deleteErr    error
	authorizedID uuid.UUID
}

func (s *bucketDeleteGuardStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	return s.accountID, nil
}

func (s *bucketDeleteGuardStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

func (s *bucketDeleteGuardStore) GetBucketByName(context.Context, string) (*store.Bucket, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	return &s.bucket, nil
}

func (s *bucketDeleteGuardStore) GetBucketConnectSummary(context.Context, uuid.UUID) (*store.BucketConnectSummary, error) {
	if s.summaryErr != nil {
		return nil, s.summaryErr
	}
	return &s.summary, nil
}

func (s *bucketDeleteGuardStore) DeleteBucket(_ context.Context, _ string, authorizedBucketID uuid.UUID) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = true
	s.deletedID = authorizedBucketID
	return nil
}

func deleteBucketForTest(t *testing.T, s *bucketDeleteGuardStore, name string) *httptest.ResponseRecorder {
	t.Helper()
	r := newControlTestRouter(s.accountID)
	r.Delete("/workspace/buckets/{name}", DeleteBucketHandler(s))
	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+name, nil)
	req.Header.Set("X-API-Key", "test-key")
	authorizedID := s.authorizedID
	if authorizedID == uuid.Nil {
		authorizedID = s.bucket.ID
	}
	req = req.WithContext(accesscontrol.ContextWithRequiredPermissions(req.Context(), []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionBucketManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: authorizedID},
	}}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}
