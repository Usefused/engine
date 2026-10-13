package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// scheduleControlFixture fails unexpected storage calls while exposing a private read for authorization tests.
type scheduleControlFixture struct {
	*unifiedAppBundleControlStore
	store.UnifiedAppScheduleStore
	reads int
}

// ListUnifiedAppSchedules detects any private read attempted before successful family authorization.
func (fixture *scheduleControlFixture) ListUnifiedAppSchedules(context.Context, uuid.UUID, uuid.UUID, []byte) ([]store.UnifiedAppSchedule, error) {
	fixture.reads++
	return []store.UnifiedAppSchedule{{ID: uuid.New(), Name: "Fixture", Input: json.RawMessage(`{"private":"saved-input"}`)}}, nil
}

// TestScheduleControlAuthorization rejects unauthenticated, read-only, and cross-account access before decrypting inputs.
func TestScheduleControlAuthorization(t *testing.T) {
	base, _, _ := newUnifiedAppBundleControlFixture(t)
	fixture := &scheduleControlFixture{unifiedAppBundleControlStore: base}
	path := "/apps/" + base.app.AppID.String() + "/schedules"
	owner := newControlTestRouter(base.app.AccountID)
	owner.Get("/apps/{app_id}/schedules", UnifiedAppSchedulesHandler(fixture, nil))
	response := httptest.NewRecorder()
	owner.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != 200 || fixture.reads != 1 {
		t.Fatalf("owner status=%d reads=%d", response.Code, fixture.reads)
	}
	// Runtime credentials do not establish a control actor or grant access to saved inputs.
	for _, account := range []uuid.UUID{uuid.Nil, base.app.AccountID, uuid.New()} {
		router := chi.NewRouter()
		router.Get("/apps/{app_id}/schedules", UnifiedAppSchedulesHandler(fixture, nil))
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer execution-token")
		// An actor with no management grant remains denied even inside the correct workspace.
		if account != uuid.Nil {
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), accesscontrol.Actor{AccountID: account, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}))
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code < 400 || fixture.reads != 1 || strings.Contains(response.Body.String(), "saved-input") {
			t.Fatalf("private read escaped: %d %s", response.Code, response.Body.String())
		}
	}
}

// TestScheduleDecodeRejectsAuthorityInjection keeps worker-owned fields and trailing JSON outside the mutation contract.
func TestScheduleDecodeRejectsAuthorityInjection(t *testing.T) {
	for _, body := range []string{`{"last_execution_id":"forged"}`, `{"account_id":"forged"}`, `{"enabled":true} {"enabled":false}`} {
		_, err := decodeUnifiedAppSchedule(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)))
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

// scheduleWorkerFixture records whether an automatic run reached effectful reservation while licensing blocks it.
type scheduleWorkerFixture struct {
	store.UnifiedAppScheduleStore
	recovered  int
	discovered int
}

// RecoverScheduledExecutions permits conservative cleanup even when execution is suspended.
func (fixture *scheduleWorkerFixture) RecoverScheduledExecutions(context.Context) ([]store.ExecutionResult, error) {
	fixture.recovered++
	return nil, nil
}

// DueUnifiedAppSchedules records discovery without involving a provider runtime.
func (fixture *scheduleWorkerFixture) DueUnifiedAppSchedules(context.Context, []byte) ([]store.UnifiedAppSchedule, error) {
	fixture.discovered++
	return nil, nil
}

// TestScheduleWorkerLicenseGate proves background execution cannot bypass the HTTP suspension and heartbeat gates.
func TestScheduleWorkerLicenseGate(t *testing.T) {
	oldSuspended, oldExpired := entitlement.EngineSuspended.Load(), entitlement.EngineHeartbeatLeaseExpired.Load()
	// Process-wide licensing state must be restored for other execution fixtures.
	t.Cleanup(func() {
		entitlement.EngineSuspended.Store(oldSuspended)
		entitlement.EngineHeartbeatLeaseExpired.Store(oldExpired)
	})
	fixture := &scheduleWorkerFixture{}
	server := &EngineGRPCServer{}
	entitlement.EngineSuspended.Store(true)
	server.runScheduleBatch(context.Background(), fixture)
	entitlement.EngineSuspended.Store(false)
	entitlement.EngineHeartbeatLeaseExpired.Store(true)
	server.runScheduleBatch(context.Background(), fixture)
	if fixture.recovered != 2 || fixture.discovered != 0 {
		t.Fatalf("license gate bypassed: %+v", fixture)
	}
	entitlement.EngineHeartbeatLeaseExpired.Store(false)
	server.runScheduleBatch(context.Background(), fixture)
	if fixture.discovered != 1 {
		t.Fatal("verified Engine did not discover schedules")
	}
}
