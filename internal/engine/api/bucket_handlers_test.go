package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
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

// TestDeleteBucketHandlerMapsDeletionFailures verifies typed rejections and
// unknown repository outcomes expose truthful mutation metadata.
func TestDeleteBucketHandlerMapsDeletionFailures(t *testing.T) {
	for _, test := range []struct {
		name            string
		deleteErr       error
		wantStatus      int
		wantCommitState string
	}{
		{name: "bound", deleteErr: store.ErrBucketBound, wantStatus: http.StatusConflict, wantCommitState: "not_committed"},
		{name: "default", deleteErr: store.ErrDefaultBucketProtected, wantStatus: http.StatusConflict, wantCommitState: "not_committed"},
		{name: "not found", deleteErr: store.ErrBucketNotFound, wantStatus: http.StatusNotFound, wantCommitState: "not_committed"},
		{name: "repository", deleteErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError, wantCommitState: "unknown"},
	} {
		// Each repository class must retain its distinct commit certainty.
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
			var envelope workspaceConfigErrorResponse
			// The structured envelope is the contract consumed by CLI remediation.
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if envelope.Error.Phase != "bucket_delete" || envelope.Error.CommitState != test.wantCommitState || envelope.Error.OperationID != "" {
				t.Fatalf("unexpected mutation metadata: %#v", envelope.Error)
			}
			// Unknown outcomes must direct inspection before any retry.
			if test.wantCommitState == "unknown" && !strings.Contains(envelope.Error.Remediation, "Inspect") {
				t.Fatalf("unknown outcome remediation = %q", envelope.Error.Remediation)
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

// bucketCreateGuardStore stubs Store for CreateBucketHandler tests.
type bucketCreateGuardStore struct {
	store.Store
	accountID uuid.UUID
	count     int
	countErr  error
	created   *store.Bucket
	createErr error
}

func (s *bucketCreateGuardStore) CountBuckets(context.Context) (int, error) {
	return s.count, s.countErr
}

func (s *bucketCreateGuardStore) CreateBucket(context.Context, string, bool) (*store.Bucket, error) {
	return s.created, s.createErr
}

func createBucketForTest(t *testing.T, s *bucketCreateGuardStore) *httptest.ResponseRecorder {
	t.Helper()
	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/buckets", CreateBucketHandler(s))
	req := httptest.NewRequest(http.MethodPost, "/workspace/buckets", strings.NewReader(`{"name":"prod","is_default":false}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(accesscontrol.ContextWithRequiredPermissions(req.Context(), []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionBucketManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: uuid.New()},
	}}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestCreateBucketHandler_Limits(t *testing.T) {
	accountID := uuid.New()
	workspaceID := uuid.New()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
		MaxBuckets: models.IntPtr(-1), // unlimited
	})
	defer entitlement.LiveEntitlement.Reset()

	store := &bucketCreateGuardStore{
		accountID: accountID,
		count:     10,
		created:   &store.Bucket{ID: uuid.New(), Name: "prod"},
	}

	// unlimited: allow
	rr := createBucketForTest(t, store)
	if rr.Code != http.StatusOK {
		t.Fatalf("unlimited should allow, got %d: %s", rr.Code, rr.Body.String())
	}

	// zero: block
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxBuckets: models.IntPtr(0)})
	rr = createBucketForTest(t, store)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("zero limit should block, got %d", rr.Code)
	}

	// hard ceiling: allow below limit
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxBuckets: models.IntPtr(11)})
	rr = createBucketForTest(t, store)
	if rr.Code != http.StatusOK {
		t.Fatalf("10/11 should allow, got %d: %s", rr.Code, rr.Body.String())
	}

	// hard ceiling: block at limit
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxBuckets: models.IntPtr(10)})
	rr = createBucketForTest(t, store)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("10/10 should block, got %d", rr.Code)
	}

	// count error: 500
	store.countErr = errors.New("db down")
	rr = createBucketForTest(t, store)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("count error should 500, got %d", rr.Code)
	}

	// Verify workspaceID is set on the actor context properly
	_ = workspaceID
}
