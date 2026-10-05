package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type diagnosticRouteStore struct {
	store.Store
	store.ExecutionResultStore
	app   store.App
	reads int
}

// GetApp supplies the exact family ownership checked before any diagnostic lookup.
func (s *diagnosticRouteStore) GetApp(context.Context, uuid.UUID) (*store.App, error) {
	return &s.app, nil
}

// GetExecutionResult provides private input only through the result store projection.
func (s *diagnosticRouteStore) GetExecutionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*store.ExecutionResult, error) {
	return &store.ExecutionResult{Input: json.RawMessage(`{"private":"input"}`), AppVersion: "1.0.0"}, nil
}

// SaveExecutionDiagnostics satisfies the optional diagnostic capability without writing data in read tests.
func (s *diagnosticRouteStore) SaveExecutionDiagnostics(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, json.RawMessage, []byte) error {
	return nil
}

// GetExecutionDiagnostics records reads so denied requests prove no private retrieval happened.
func (s *diagnosticRouteStore) GetExecutionDiagnostics(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte) (json.RawMessage, error) {
	s.reads++
	return json.RawMessage(`{"error":{"phase":"execute","message":"private exception"},"calls":[]}`), nil
}

// TestExecutionDiagnosticsPermissionBoundary enforces family grants independently from normal app reads.
func TestExecutionDiagnosticsPermissionBoundary(t *testing.T) {
	account, family, appID := uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name        string
		permission  accesscontrol.Permission
		grantFamily uuid.UUID
		foreign     bool
		status      int
	}{
		{"ordinary read", accesscontrol.PermissionAppUnifiedAppRead, family, false, 403},
		{"explicit diagnostic grant", accesscontrol.PermissionUnifiedAppDiagnosticsRead, family, false, 200},
		{"other family", accesscontrol.PermissionUnifiedAppDiagnosticsRead, uuid.New(), false, 403},
		{"other account", accesscontrol.PermissionUnifiedAppDiagnosticsRead, family, true, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &diagnosticRouteStore{app: store.App{AppID: appID, AppFamilyID: family, AccountID: account}}
			actor := controlTestOwnerActor(account)
			snapshot, err := appPermissionTestSnapshot(1, accesscontrol.Grant{Permission: tc.permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: tc.grantFamily}})
			// The fixture must have a valid authorization snapshot before exercising the route.
			if err != nil {
				t.Fatal(err)
			}
			actor.Authorization = snapshot
			// A valid grant in one account cannot open a different account's version.
			if tc.foreign {
				actor.AccountID = uuid.New()
			}
			router := chi.NewRouter()
			router.Get("/apps/{app_id}/executions/{execution_id}/diagnostics", ExecutionDiagnosticsHandler(fixture, nil))
			request := httptest.NewRequest(http.MethodGet, "/apps/"+appID.String()+"/executions/"+uuid.New().String()+"/diagnostics", nil)
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// Status alone is insufficient: a rejected request must never load or disclose private bytes.
			if response.Code != tc.status || (tc.status != 200 && (fixture.reads != 0 || strings.Contains(response.Body.String(), "private"))) {
				t.Fatalf("status %d reads %d body %s", response.Code, fixture.reads, response.Body.String())
			}
			// Successful reads deliberately expose detailed errors and forbid shared HTTP caching.
			if tc.status == 200 && (!strings.Contains(response.Body.String(), "private exception") || response.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("missing details/cache isolation: %s", response.Body.String())
			}
		})
	}
}

// TestUnifiedAppFailureReceiptIncludesRunsWithoutProviders keeps authored exceptions visible in canonical activity.
func TestUnifiedAppFailureReceiptIncludesRunsWithoutProviders(t *testing.T) {
	now := time.Now()
	record := &store.ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppTokenID: uuid.New(), AppVersion: "1.0.0", CreatedAt: now.Add(-time.Second), CompletedAt: &now, Status: "failed", ErrorCode: "input_validation_failed", Input: json.RawMessage(`{"private":"value"}`)}
	event := unifiedAppReceipt(context.Background(), record, capabilityRunSpec{transport: "mcp"})
	raw, _ := json.Marshal(event)
	// A parent without physical attempts must still be admitted by the canonical event schema.
	if err := executionevent.ValidateUnifiedMetadata(event); err != nil {
		t.Fatal(err)
	}
	// Counts and identity survive without copying input or private error data into analytics.
	if event.ID != record.ID || event.Status != "failed" || event.Transport != "mcp" || event.AttemptCount != 0 || strings.Contains(string(raw), "private") {
		t.Fatalf("unsafe or incomplete receipt: %s", raw)
	}
}

// TestProviderDiagnosticsPreserveRejectedBodyWithoutReplayLeak keeps failed HTTP bodies useful only to privileged readers.
func TestProviderDiagnosticsPreserveRejectedBodyWithoutReplayLeak(t *testing.T) {
	recorder := newReplayTestRecorder(t, &diagnosticProviderHost{})
	_, err := recorder.Fetch(context.Background(), json.RawMessage(`{"input":{"customer":"example"},"operation":"read","service":"crm"}`))
	detail := recorder.diagnostics(err)
	history, historyErr := recorder.History()
	// Replay retains the selected explanation, while raw bodies remain only in private diagnostics.
	if !strings.Contains(string(detail), "provider private body") || !strings.Contains(string(detail), "provider error explanation") || strings.Contains(err.Error(), "private") || historyErr != nil || strings.Contains(string(history), "provider private body") {
		t.Fatalf("bad diagnostic separation: %s / %s / %v", detail, history, err)
	}
}

type diagnosticProviderHost struct{ replayHostFixture }

// Fetch simulates a rejected provider response without granting a real outbound effect.
func (*diagnosticProviderHost) Fetch(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, &executionappvm.DiagnosticError{Phase: "provider", Message: "provider error explanation", Response: "provider private body"}
}
