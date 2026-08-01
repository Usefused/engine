package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type auditExportStore struct {
	store.Store
	rows  []store.AuditExportRow
	err   error
	query store.AuditExportQuery
	calls int
}

func (s *auditExportStore) QueryAuditEvents(context.Context, store.AuditQuery) (store.AuditPage, error) {
	return store.AuditPage{}, nil
}

func (s *auditExportStore) ExportAuditEvents(_ context.Context, query store.AuditExportQuery) ([]store.AuditExportRow, error) {
	s.calls++
	s.query = query
	return s.rows, s.err
}

func TestAuditExportHandlerProducesBoundedSafeCSV(t *testing.T) {
	workspaceID, actorID, credentialID := uuid.New(), uuid.New(), uuid.New()
	repository := &auditExportStore{rows: []store.AuditExportRow{{
		ID: uuid.New(), OccurredAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), ActorSubjectID: &actorID, ActorCredentialID: &credentialID,
		Action: "=WEBSERVICE(\"https://invalid.example\")", Permission: accesscontrol.PermissionAccessManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		RequestID: "request-safe", TraceID: "trace-safe", Method: http.MethodPost, Path: "/engine/graphql", Outcome: accesscontrol.AuditDenied, StatusCode: http.StatusForbidden,
		MissingRequirements: []accesscontrol.Requirement{{Permission: accesscontrol.PermissionCredentialsManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/audit/export?format=csv&action=team.update,team.create&outcome=attempted&limit=25", nil)
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), controlTestOwnerActor(workspaceID)))
	response := httptest.NewRecorder()

	AuditExportHandler(repository).ServeHTTP(response, request)

	if response.Code != http.StatusOK || repository.calls != 1 || repository.query.Limit != 25 {
		t.Fatalf("status/calls/query = %d/%d/%#v; body=%s", response.Code, repository.calls, repository.query, response.Body.String())
	}
	if len(repository.query.Actions) != 2 || len(repository.query.Outcomes) != 1 || repository.query.Outcomes[0] != accesscontrol.AuditAttempted {
		t.Fatalf("query filters = %#v", repository.query)
	}
	assertSafeAuditExport(t, response.Body.String())
	if !strings.Contains(response.Body.String(), `'=WEBSERVICE`) {
		t.Fatalf("CSV formula prefix was not neutralized: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "credentials.manage") {
		t.Fatalf("CSV omitted missing requirements: %s", response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/csv") || !strings.Contains(response.Header().Get("Content-Disposition"), "fused-audit.csv") {
		t.Fatalf("export headers = %#v", response.Header())
	}
}

func TestAuditExportHandlerProducesSafeJSONL(t *testing.T) {
	workspaceID := uuid.New()
	repository := &auditExportStore{rows: []store.AuditExportRow{{
		ID: uuid.New(), OccurredAt: time.Now().UTC(), Action: "access.read", Outcome: accesscontrol.AuditDenied,
		MissingRequirements: []accesscontrol.Requirement{{Permission: accesscontrol.PermissionAccessRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/audit/export?format=jsonl", nil)
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), controlTestOwnerActor(workspaceID)))
	response := httptest.NewRecorder()

	AuditExportHandler(repository).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var row map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode JSONL row: %v", err)
	}
	assertSafeAuditExport(t, response.Body.String())
	if _, exists := row["metadata"]; exists {
		t.Fatalf("JSONL export exposed metadata: %#v", row)
	}
	missing, ok := row["missing_requirements"].([]interface{})
	if !ok || len(missing) != 1 || missing[0].(map[string]interface{})["permission"] != string(accesscontrol.PermissionAccessRead) {
		t.Fatalf("JSONL missing requirements = %#v", row["missing_requirements"])
	}
}

func TestAuditExportHandlerRejectsInvalidInputsBeforeRepositoryCall(t *testing.T) {
	workspaceID := uuid.New()
	for _, path := range []string{
		"/audit/export?format=xml",
		"/audit/export?limit=10001",
		"/audit/export?outcome=unknown",
		"/audit/export?actor_subject_id=not-a-uuid",
	} {
		t.Run(path, func(t *testing.T) {
			repository := &auditExportStore{}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), controlTestOwnerActor(workspaceID)))
			response := httptest.NewRecorder()
			AuditExportHandler(repository).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || repository.calls != 0 {
				t.Fatalf("status/calls = %d/%d; body=%s", response.Code, repository.calls, response.Body.String())
			}
		})
	}
}

func assertSafeAuditExport(t *testing.T, body string) {
	t.Helper()
	for _, forbidden := range []string{"metadata", "source_ip", "user_agent", "request_body", "response_body", "fsk_"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("export contains forbidden %q: %s", forbidden, body)
		}
	}
}
