package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// recoveryControlFixture detects any history read before management authorization succeeds.
type recoveryControlFixture struct {
	*unifiedAppBundleControlStore
	store.ExecutionResultStore
	store.ExecutionRecoveryStore
	reads int
}

// GetExecutionResult supplies only fixture-owned identity after the handler's family authorization check.
func (fixture *recoveryControlFixture) GetExecutionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*store.ExecutionResult, error) {
	fixture.reads++
	return &store.ExecutionResult{AccountID: fixture.app.AccountID, AppID: fixture.app.AppID}, nil
}

// ListExecutionAttempts returns metadata without granting access to snapshot values.
func (*recoveryControlFixture) ListExecutionAttempts(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]store.ExecutionAttemptSummary, error) {
	return []store.ExecutionAttemptSummary{}, nil
}

// TestRecoveryControlAuthorization prevents runtime credentials and unrelated actors from inspecting or triggering private runs.
func TestRecoveryControlAuthorization(t *testing.T) {
	base, _, _ := newUnifiedAppBundleControlFixture(t)
	fixture := &recoveryControlFixture{unifiedAppBundleControlStore: base}
	path := "/apps/" + base.app.AppID.String() + "/executions/" + uuid.NewString() + "/attempts"
	owner := newControlTestRouter(base.app.AccountID)
	owner.Get("/apps/{app_id}/executions/{execution_id}/attempts", UnifiedAppRecoveryControlHandler(fixture))
	response := httptest.NewRecorder()
	owner.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	// An authorized app owner can inspect metadata through the normal management route.
	if response.Code != http.StatusOK || fixture.reads != 1 {
		t.Fatalf("owner status=%d reads=%d", response.Code, fixture.reads)
	}
	// Tokens, unprivileged workspace actors and foreign accounts all stop before retained result lookup.
	for _, account := range []uuid.UUID{uuid.Nil, base.app.AccountID, uuid.New()} {
		router := chi.NewRouter()
		router.Get("/apps/{app_id}/executions/{execution_id}/attempts", UnifiedAppRecoveryControlHandler(fixture))
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer execution-token")
		// A control actor's presence alone does not imply a family grant.
		if account != uuid.Nil {
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), accesscontrol.Actor{AccountID: account, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}))
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code < 400 || fixture.reads != 1 {
			t.Fatalf("unauthorized read status=%d reads=%d", response.Code, fixture.reads)
		} // No rejected caller may reach retained input or progress metadata.
	}
}
