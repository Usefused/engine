package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type sdkGenerationFinalizerStoreFixture struct {
	mu            sync.Mutex
	build         store.SDKGenerationBuild
	terminal      bool
	completeCalls int
	failCalls     int
	completeErr   error
}

// ListPendingSDKGenerationBuilds returns the retained row until one terminal
// compare-and-swap wins, while preserving keyset page semantics.
func (s *sdkGenerationFinalizerStoreFixture) ListPendingSDKGenerationBuilds(_ context.Context, after uuid.UUID, _ int) ([]store.SDKGenerationBuild, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Terminal rows and rows at or before the cursor are excluded by the production SQL query.
	if s.terminal || after == s.build.Request.AppID {
		return nil, nil
	}
	return []store.SDKGenerationBuild{s.build}, nil
}

// CompleteSDKGeneration models the production job-bound CAS and records only
// the exact winner.
func (s *sdkGenerationFinalizerStoreFixture) CompleteSDKGeneration(_ context.Context, appID uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Capacity reacquisition can temporarily block activation while preserving the pending build.
	if s.completeErr != nil {
		return false, s.completeErr
	}
	// A stale job or already-terminal row cannot activate this immutable version.
	if s.terminal || appID != s.build.Request.AppID || jobID != s.build.JobID || idempotencyKey != s.build.Request.IdempotencyKey {
		return false, nil
	}
	s.terminal = true
	s.completeCalls++
	return true, nil
}

// FailSDKGeneration models the terminal failure CAS without making the app runnable.
func (s *sdkGenerationFinalizerStoreFixture) FailSDKGeneration(_ context.Context, appID uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A stale job or already-terminal row cannot replace the current state.
	if s.terminal || appID != s.build.Request.AppID || jobID != s.build.JobID || idempotencyKey != s.build.Request.IdempotencyKey {
		return false, nil
	}
	s.terminal = true
	s.failCalls++
	return true, nil
}

type sdkGenerationFinalizerClientFixture struct {
	mu        sync.Mutex
	responses []models.SDKGenerationResult
	calls     int
}

// GenerateSDK returns ordered durable Registry states so tests can prove both
// startup and periodic passes use the identical retained request.
func (c *sdkGenerationFinalizerClientFixture) GenerateSDK(_ context.Context, _ models.SDKGenerationRequest) (models.SDKGenerationResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Exhaustion is a fixture failure rather than an invented Registry state.
	if c.calls >= len(c.responses) {
		return models.SDKGenerationResult{}, errors.New("unexpected generation replay")
	}
	result := c.responses[c.calls]
	c.calls++
	return result, nil
}

// TestSDKGenerationFinalizerRecoversAcrossStartupAndPeriodicPass proves a
// pending startup observation is activated by the next bounded poll.
func TestSDKGenerationFinalizerRecoversAcrossStartupAndPeriodicPass(t *testing.T) {
	build, accountID := sdkGenerationFinalizerBuildFixture()
	pending := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusPending)
	complete := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	storeFixture := &sdkGenerationFinalizerStoreFixture{build: build}
	client := &sdkGenerationFinalizerClientFixture{responses: []models.SDKGenerationResult{pending, complete}}
	activated := make(chan uuid.UUID, 1)
	worker := NewSDKGenerationFinalizer(storeFixture, client, SDKGenerationFinalizerOptions{
		Interval: 5 * time.Millisecond, RequestTimeout: time.Second, BatchSize: 10,
		OnActivated: func(_ context.Context, appID uuid.UUID) { activated <- appID },
	})
	worker.Start(t.Context())
	select {
	case appID := <-activated:
		// Only the exact building version may become runnable.
		if appID != build.Request.AppID {
			t.Fatalf("activated app ID = %s", appID)
		}
	case <-time.After(time.Second):
		t.Fatal("SDK generation finalizer did not complete the periodic replay")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	worker.Stop(stopCtx)
	// One startup observation plus one periodic completion proves both schedule phases ran.
	if client.calls != 2 || storeFixture.completeCalls != 1 || storeFixture.failCalls != 0 {
		t.Fatalf("calls = client %d, complete %d, failed %d", client.calls, storeFixture.completeCalls, storeFixture.failCalls)
	}
}

// TestSDKGenerationFinalizerFailsClosedOnScopeMismatch proves a Registry replay
// cannot replace the concrete selections admitted during apply.
func TestSDKGenerationFinalizerFailsClosedOnScopeMismatch(t *testing.T) {
	build, accountID := sdkGenerationFinalizerBuildFixture()
	result := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	result.Selections = nil
	storeFixture := &sdkGenerationFinalizerStoreFixture{build: build}
	worker := NewSDKGenerationFinalizer(storeFixture, &sdkGenerationFinalizerClientFixture{responses: []models.SDKGenerationResult{result}}, SDKGenerationFinalizerOptions{})
	_, err := worker.finalizeBuild(t.Context(), &build)
	// Mismatched scope remains pending and never runs cache invalidation or activation.
	if err == nil || storeFixture.completeCalls != 0 || storeFixture.failCalls != 0 {
		t.Fatalf("finalize mismatched scope error = %v, complete = %d, failed = %d", err, storeFixture.completeCalls, storeFixture.failCalls)
	}
}

// TestSDKGenerationFinalizerRejectsEveryIdentityMismatch proves account, app,
// family, and Registry job are all exact activation fences.
func TestSDKGenerationFinalizerRejectsEveryIdentityMismatch(t *testing.T) {
	build, accountID := sdkGenerationFinalizerBuildFixture()
	wrongAccount := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	wrongAccount.AccountID = uuid.New()
	wrongApp := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	wrongApp.AppID = uuid.New()
	wrongFamily := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	wrongFamily.AppFamilyID = uuid.New()
	wrongJob := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	wrongJob.JobID = "job-2"
	for _, test := range []struct {
		name   string
		result models.SDKGenerationResult
	}{
		{name: "account", result: wrongAccount},
		{name: "app", result: wrongApp},
		{name: "family", result: wrongFamily},
		{name: "job", result: wrongJob},
	} {
		// Each subtest runs a fresh durable store so one mismatch cannot mask another.
		t.Run(test.name, func(t *testing.T) {
			storeFixture := &sdkGenerationFinalizerStoreFixture{build: build}
			worker := NewSDKGenerationFinalizer(storeFixture, &sdkGenerationFinalizerClientFixture{responses: []models.SDKGenerationResult{test.result}}, SDKGenerationFinalizerOptions{})
			_, err := worker.finalizeBuild(t.Context(), &build)
			// Every mismatched identity must leave both terminal CAS paths untouched.
			if err == nil || storeFixture.completeCalls != 0 || storeFixture.failCalls != 0 {
				t.Fatalf("identity mismatch error = %v, complete = %d, failed = %d", err, storeFixture.completeCalls, storeFixture.failCalls)
			}
		})
	}
}

// TestSDKGenerationFinalizerPersistsConfirmedFailure proves a failed Registry
// job becomes terminal without activating the version.
func TestSDKGenerationFinalizerPersistsConfirmedFailure(t *testing.T) {
	build, accountID := sdkGenerationFinalizerBuildFixture()
	result := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusFailed)
	storeFixture := &sdkGenerationFinalizerStoreFixture{build: build}
	worker := NewSDKGenerationFinalizer(storeFixture, &sdkGenerationFinalizerClientFixture{responses: []models.SDKGenerationResult{result}}, SDKGenerationFinalizerOptions{})
	outcome, err := worker.finalizeBuild(t.Context(), &build)
	// Confirmed failure is terminal but must not call the completion transition.
	if err != nil || outcome != models.SDKGenerationStatusFailed || storeFixture.failCalls != 1 || storeFixture.completeCalls != 0 {
		t.Fatalf("failed outcome = %q, error = %v, complete = %d, failed = %d", outcome, err, storeFixture.completeCalls, storeFixture.failCalls)
	}
}

// TestSDKGenerationFinalizerRetriesBlockedActivation proves quota reacquisition
// failure leaves a completed package pending rather than misclassifying it as failed.
func TestSDKGenerationFinalizerRetriesBlockedActivation(t *testing.T) {
	build, accountID := sdkGenerationFinalizerBuildFixture()
	result := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	storeFixture := &sdkGenerationFinalizerStoreFixture{build: build, completeErr: errors.New("SDK family capacity unavailable")}
	activated := false
	worker := NewSDKGenerationFinalizer(storeFixture, &sdkGenerationFinalizerClientFixture{responses: []models.SDKGenerationResult{result}}, SDKGenerationFinalizerOptions{
		OnActivated: func(context.Context, uuid.UUID) { activated = true },
	})
	_, err := worker.finalizeBuild(t.Context(), &build)
	// The worker must retry the activation CAS later without failure state or cache invalidation.
	if err == nil || storeFixture.terminal || storeFixture.failCalls != 0 || activated {
		t.Fatalf("blocked activation error = %v, terminal = %t, failed = %d, activated = %t", err, storeFixture.terminal, storeFixture.failCalls, activated)
	}
}

// TestSDKGenerationFinalizerSuppressesLostCASOutcome proves a stale worker
// neither invalidates caches nor records another worker's terminal transition.
func TestSDKGenerationFinalizerSuppressesLostCASOutcome(t *testing.T) {
	build, accountID := sdkGenerationFinalizerBuildFixture()
	result := sdkGenerationFinalizerResult(build, accountID, models.SDKGenerationStatusComplete)
	storeFixture := &sdkGenerationFinalizerStoreFixture{build: build, terminal: true}
	activated := false
	worker := NewSDKGenerationFinalizer(storeFixture, &sdkGenerationFinalizerClientFixture{responses: []models.SDKGenerationResult{result}}, SDKGenerationFinalizerOptions{
		OnActivated: func(context.Context, uuid.UUID) { activated = true },
	})
	outcome, err := worker.finalizeBuild(t.Context(), &build)
	// CAS loss leaves authoritative state to the winner and produces no stale callback or outcome.
	if err != nil || outcome != "" || activated || storeFixture.completeCalls != 0 {
		t.Fatalf("lost CAS outcome = %q, error = %v, activated = %t, complete = %d", outcome, err, activated, storeFixture.completeCalls)
	}
}

// sdkGenerationFinalizerBuildFixture creates one self-consistent retained
// request with a concrete v3 selection.
func sdkGenerationFinalizerBuildFixture() (store.SDKGenerationBuild, uuid.UUID) {
	appID, familyID, accountID := uuid.New(), uuid.New(), uuid.New()
	selection := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SchemaVersion: models.AppSelectionSchemaVersion}
	return store.SDKGenerationBuild{
		AccountID: accountID,
		Request: models.SDKGenerationRequest{
			AppID: appID, AppFamilyID: familyID, GeneratorVersion: models.SDKGeneratorVersion,
			Selections: []models.SDKSelection{selection}, IdempotencyKey: uuid.NewString(),
		},
		JobID: "job-1", Status: models.SDKGenerationStatusPending,
	}, accountID
}

// sdkGenerationFinalizerResult mirrors the retained request while varying only
// the Registry-owned job status under test.
func sdkGenerationFinalizerResult(build store.SDKGenerationBuild, accountID uuid.UUID, status string) models.SDKGenerationResult {
	return models.SDKGenerationResult{
		AppID: build.Request.AppID, AppFamilyID: build.Request.AppFamilyID, AccountID: accountID,
		JobID: build.JobID, Status: status, ScopeSchemaVersion: models.AppScopeSchemaVersion,
		GeneratorVersion: build.Request.GeneratorVersion, Selections: models.SDKSelections(build.Request.Selections),
	}
}
