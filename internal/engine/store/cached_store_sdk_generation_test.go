package store

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type cachedSDKGenerationDelegate struct {
	Store
	build         SDKGenerationBuild
	completedJob  string
	failedJob     string
	requestedApp  uuid.UUID
	requestedAcct uuid.UUID
}

// GetSDKGenerationBuild records exact recovery identity and returns the retained request.
func (d *cachedSDKGenerationDelegate) GetSDKGenerationBuild(_ context.Context, accountID, appID uuid.UUID) (*SDKGenerationBuild, error) {
	d.requestedAcct, d.requestedApp = accountID, appID
	return &d.build, nil
}

// ListPendingSDKGenerationBuilds returns one build so the wrapper can prove optional capability forwarding.
func (d *cachedSDKGenerationDelegate) ListPendingSDKGenerationBuilds(_ context.Context, _ uuid.UUID, _ int) ([]SDKGenerationBuild, error) {
	return []SDKGenerationBuild{d.build}, nil
}

// CompleteSDKGeneration records the exact job-bound activation transition.
func (d *cachedSDKGenerationDelegate) CompleteSDKGeneration(_ context.Context, _ uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	d.completedJob = jobID + ":" + idempotencyKey
	return true, nil
}

// FailSDKGeneration records the exact job-bound terminal failure transition.
func (d *cachedSDKGenerationDelegate) FailSDKGeneration(_ context.Context, _ uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	d.failedJob = jobID + ":" + idempotencyKey
	return true, nil
}

// TestCachedStoreForwardsSDKGenerationBuildCapability proves the production
// cache wrapper does not hide durable finalization from handlers or workers.
func TestCachedStoreForwardsSDKGenerationBuildCapability(t *testing.T) {
	appID, familyID, accountID := uuid.New(), uuid.New(), uuid.New()
	delegate := &cachedSDKGenerationDelegate{build: SDKGenerationBuild{
		AccountID: accountID,
		Request:   models.SDKGenerationRequest{AppID: appID, AppFamilyID: familyID},
		JobID:     "job-1", Status: models.SDKGenerationStatusPending,
	}}
	cached, ok := NewCachedStore(delegate, nil).(SDKGenerationBuildStore)
	// Production construction must retain the optional durable build capability.
	if !ok {
		t.Fatal("cached store does not implement SDKGenerationBuildStore")
	}
	build, err := cached.GetSDKGenerationBuild(t.Context(), accountID, appID)
	// Exact account and app identity must reach the durable delegate unchanged.
	if err != nil || build.JobID != "job-1" || delegate.requestedAcct != accountID || delegate.requestedApp != appID {
		t.Fatalf("GetSDKGenerationBuild() = %+v, %v; requested %s/%s", build, err, delegate.requestedAcct, delegate.requestedApp)
	}
	page, err := cached.ListPendingSDKGenerationBuilds(t.Context(), uuid.Nil, 10)
	// Pending discovery must not be served from the runtime cache.
	if err != nil || len(page) != 1 || page[0].Request.AppID != appID {
		t.Fatalf("ListPendingSDKGenerationBuilds() = %+v, %v", page, err)
	}
	completed, completeErr := cached.CompleteSDKGeneration(t.Context(), appID, "job-1", "attempt-1")
	failed, failErr := cached.FailSDKGeneration(t.Context(), appID, "job-2", "attempt-2")
	// Both terminal CAS operations must preserve Registry job and Engine attempt identity.
	if completeErr != nil || failErr != nil || !completed || !failed || delegate.completedJob != "job-1:attempt-1" || delegate.failedJob != "job-2:attempt-2" {
		t.Fatalf("terminal forwarding = completed %t/%v %q, failed %t/%v %q", completed, completeErr, delegate.completedJob, failed, failErr, delegate.failedJob)
	}
}
