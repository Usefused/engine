package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

type controlAuditRecorderStub struct {
	events             []accesscontrol.AuditEvent
	eventIDs           []uuid.UUID
	err                error
	failAtCall         int
	failFromCall       int
	requireLiveContext bool
	calls              int
}

func (recorder *controlAuditRecorderStub) RecordAuthorizationAudit(ctx context.Context, event accesscontrol.AuditEvent) error {
	recorder.calls++
	recorder.eventIDs = append(recorder.eventIDs, event.ID)
	if recorder.requireLiveContext && ctx.Err() != nil {
		return ctx.Err()
	}
	if recorder.failFromCall > 0 && recorder.calls >= recorder.failFromCall {
		return errors.New("audit unavailable")
	}
	if recorder.failAtCall == recorder.calls {
		return errors.New("audit unavailable")
	}
	if recorder.err != nil {
		return recorder.err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	recorder.events = append(recorder.events, event)
	return nil
}

func TestControlAuditRecordsAuthenticationDenial(t *testing.T) {
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{}
	handler := controlActorMiddlewareWithAudit(authenticator, recorder)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not execute")
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/account", nil))

	if response.Code != http.StatusUnauthorized || len(recorder.events) != 1 || recorder.events[0].Outcome != accesscontrol.AuditDenied {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
}

func TestControlAuthenticationAuditUsesSafeRouteFamily(t *testing.T) {
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	tests := []struct {
		name          string
		path          string
		authenticator *accesscontrol.Authenticator
	}{
		{name: "embedded credential shape", path: "/integrations/fsk_secret_that_must_not_persist", authenticator: authenticator},
		{name: "overlong path", path: "/integrations/" + strings.Repeat("x", 300), authenticator: authenticator},
		{name: "unavailable authenticator", path: "/account/fused_secret_that_must_not_persist"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &controlAuditRecorderStub{}
			handler := controlActorMiddlewareWithAudit(test.authenticator, recorder)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("handler must not execute")
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusUnauthorized || len(recorder.events) != 1 {
				t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
			}
			if event := recorder.events[0]; event.Path != controlAuditRouteFamily(test.path) || strings.Contains(event.Path, "secret") || len(event.Path) > 256 {
				t.Fatalf("unsafe authentication audit path = %q", event.Path)
			}
		})
	}
}

func TestControlAuditRecordsMutationOutcome(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{}
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	if recorder.events[0].Outcome != accesscontrol.AuditAttempted {
		t.Fatalf("preflight event = %#v", recorder.events[0])
	}
	event := recorder.events[1]
	if event.Outcome != accesscontrol.AuditSucceeded || event.Permission != accesscontrol.PermissionAccountManage || event.Path != "/account" {
		t.Fatalf("event = %#v", event)
	}
	if _, present := event.Metadata["changed"]; present {
		t.Fatalf("ordinary mutation must not invent change evidence: %#v", event)
	}
}

func TestControlAuditRecordsBoundedNoopEvidenceOnFinalOutcome(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{}
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accesscontrol.MarkMutationAuditUnchanged(r.Context())
			w.WriteHeader(http.StatusNoContent)
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	if metadata := recorder.events[1].Metadata; recorder.events[1].Outcome != accesscontrol.AuditSucceeded || metadata["changed"] != false {
		t.Fatalf("final no-op audit = %#v", recorder.events[1])
	}
	if _, present := recorder.events[0].Metadata["changed"]; present {
		t.Fatalf("preflight audit must not claim a mutation result: %#v", recorder.events[0])
	}
}

func TestControlAuditRecordsRolledBackMutationOutcome(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	})
	recorder := &controlAuditRecorderStub{}
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accesscontrol.MarkMutationAuditRolledBack(r.Context())
		w.WriteHeader(http.StatusConflict)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, http.MethodPut, "/account", actor))

	if response.Code != http.StatusConflict || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	final := recorder.events[1]
	if final.Outcome != accesscontrol.AuditRolledBack || final.ReasonCode != "transaction_rolled_back" {
		t.Fatalf("rolled-back audit = %#v", final)
	}
}

func TestControlAuditRecordsCancelledMutationAndRollbackFact(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	})
	recorder := &controlAuditRecorderStub{}
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accesscontrol.MarkMutationAuditCancelled(r.Context())
		accesscontrol.MarkMutationAuditRolledBack(r.Context())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, http.MethodPut, "/account", actor))

	if response.Code != http.StatusInternalServerError || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	final := recorder.events[1]
	if final.Outcome != accesscontrol.AuditCancelled || final.ReasonCode != "transaction_rolled_back" {
		t.Fatalf("cancelled audit = %#v", final)
	}
}

func TestControlAuditRecordsExactFailedImportApplyOutcome(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionCatalogueImport,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	})
	recorder := &controlAuditRecorderStub{}
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, http.MethodPost, "/integrations/import/apply", actor))

	if response.Code != http.StatusConflict || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	final := recorder.events[1]
	if final.Outcome != accesscontrol.AuditFailed || final.StatusCode != http.StatusConflict || final.Path != "/integrations/import/apply" {
		t.Fatalf("failed import audit = %#v", final)
	}
}

func TestControlAuditFailureBlocksMutationBeforeExecution(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{err: errors.New("audit unavailable")}
	executed := false
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			executed = true
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || executed {
		t.Fatalf("status/executed = %d/%v", response.Code, executed)
	}
}

func TestControlAuditNilRecorderBlocksMutationBeforeExecution(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	executed := false
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			executed = true
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || executed {
		t.Fatalf("status/executed = %d/%v", response.Code, executed)
	}
}

func TestControlAuditFinalFailurePreservesBufferedMutationSuccess(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{failFromCall: 2}
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || recorder.calls != 3 || len(recorder.events) != 1 {
		t.Fatalf("status/calls/events = %d/%d/%#v", response.Code, recorder.calls, recorder.events)
	}
	if recorder.events[0].Outcome != accesscontrol.AuditAttempted {
		t.Fatalf("durable preflight = %#v", recorder.events[0])
	}
}

func TestControlAuditFinalizationRetriesWithStableEventID(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionAccountManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
	recorder := &controlAuditRecorderStub{failAtCall: 2}
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, http.MethodPut, "/account", actor))
	if response.Code != http.StatusNoContent || recorder.calls != 3 || len(recorder.events) != 2 || recorder.events[1].ID == uuid.Nil || recorder.eventIDs[1] != recorder.eventIDs[2] {
		t.Fatalf("status/calls/events = %d/%d/%#v", response.Code, recorder.calls, recorder.events)
	}
}

func TestControlGraphQLAuditFailureBlocksMutationBeforeExecution(t *testing.T) {
	recorder := &controlAuditRecorderStub{err: errors.New("audit unavailable")}
	executed := false
	handler := controlGraphQLAuditMiddleware(recorder)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		executed = true
	}))
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"mutation { updateServicePublic(serviceId: \"id\", isPublic: true) }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), controlAuditTestActor(t)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || executed {
		t.Fatalf("status/executed = %d/%v", response.Code, executed)
	}
}

func TestControlGraphQLAuditNilRecorderBlocksMutation(t *testing.T) {
	executed := false
	handler := controlGraphQLAuditMiddleware(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		executed = true
	}))
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"mutation { updateServicePublic(serviceId: \"id\", isPublic: true) }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), controlAuditTestActor(t)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || executed {
		t.Fatalf("status/executed = %d/%v", response.Code, executed)
	}
}

func TestControlGraphQLFinalAuditFailurePreservesBufferedSuccess(t *testing.T) {
	recorder := &controlAuditRecorderStub{failFromCall: 2}
	handler := controlGraphQLAuditMiddleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createTeam":{"changed":true}}}`))
	}))
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"mutation { createTeam(input: {name: \"Platform\"}) { changed } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), controlAuditTestActor(t)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || recorder.calls != 3 || len(recorder.events) != 1 || !strings.Contains(response.Body.String(), "createTeam") {
		t.Fatalf("status/calls/events = %d/%d/%#v", response.Code, recorder.calls, recorder.events)
	}
	if recorder.events[0].ReasonCode != "attempted" || recorder.events[0].Outcome != accesscontrol.AuditAttempted {
		t.Fatalf("GraphQL attempt receipt = %#v", recorder.events[0])
	}
}

func TestControlAuditRecordsAuthenticatedPermissionDenial(t *testing.T) {
	principal := controlTestPrincipal()
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{}
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("handler must not execute")
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(recorder.events) != 1 || recorder.events[0].Outcome != accesscontrol.AuditDenied {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
}

func TestControlGraphQLAuditClassifiesMutationAndGraphQLError(t *testing.T) {
	actor := controlAuditTestActor(t)
	tests := []struct {
		name        string
		body        string
		response    string
		wantEvents  int
		wantOutcome accesscontrol.AuditOutcome
	}{
		{name: "ordinary read is not persisted", body: `{"query":"query Read { services { id } }"}`, response: `{"data":{}}`},
		{name: "ordinary permission denial is persisted", body: `{"query":"query Read { services { id } }"}`, response: `{"error":"forbidden"}`, wantEvents: 1, wantOutcome: accesscontrol.AuditDenied},
		{name: "selected mutation", body: `{"query":"query Read { services { id } } mutation Write { updateServicePublic(serviceId: \"id\", isPublic: true) }","operationName":"Write"}`, response: `{"data":{}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "sensitive GraphQL error", body: `{"query":"{ bucketValues(bucket_id: \"id\") { value } }"}`, response: `{"errors":[{"message":"failed"}]}`, wantEvents: 2, wantOutcome: accesscontrol.AuditFailed},
		{name: "people access read", body: `{"query":"{ users { total } }"}`, response: `{"data":{"users":{"total":0}}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "effective access read through fragment", body: `{"query":"query Access { ...AccessFields } fragment AccessFields on Query { userEffectiveAccess(user_id: \"00000000-0000-0000-0000-000000000001\") { authorization_revision } }"}`, response: `{"data":{}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "team membership read", body: `{"query":"{ teamMembers(team_id: \"00000000-0000-0000-0000-000000000001\") { total } }"}`, response: `{"data":{}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "access explanation read", body: `{"query":"{ accessExplanation(target_subject_id: \"00000000-0000-0000-0000-000000000001\", permission: \"workspace.read\", resource_type: WORKSPACE, resource_id: \"00000000-0000-0000-0000-000000000002\") { allowed } }"}`, response: `{"data":{}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "audit events through fragment", body: `{"query":"query Audit { ...AuditFields } fragment AuditFields on Query { auditEvents { total } }"}`, response: `{"data":{}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "connection literal read", body: `{"query":"{ workspaceConnectionProfile(service_id: \"00000000-0000-0000-0000-000000000001\") { bindings { literal_value } } }"}`, response: `{"data":{}}`, wantEvents: 2, wantOutcome: accesscontrol.AuditSucceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &controlAuditRecorderStub{}
			handler := controlGraphQLAuditMiddleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.wantOutcome == accesscontrol.AuditDenied {
					w.WriteHeader(http.StatusForbidden)
				}
				_, _ = w.Write([]byte(test.response))
			}))
			request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(test.body))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if len(recorder.events) != test.wantEvents {
				t.Fatalf("events = %#v", recorder.events)
			}
			if test.wantEvents > 0 && recorder.events[len(recorder.events)-1].Outcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", recorder.events[len(recorder.events)-1].Outcome, test.wantOutcome)
			}
		})
	}
}

func TestControlGraphQLAuditParsesChunkedEnvelopeWithoutSubstringFalsePositives(t *testing.T) {
	actor := controlAuditTestActor(t)
	for _, test := range []struct {
		name        string
		chunks      []string
		wantOutcome accesscontrol.AuditOutcome
	}{
		{name: "errors key split across writes", chunks: []string{`{"err`, `ors":[{"message":"failed"}]}`}, wantOutcome: accesscontrol.AuditFailed},
		{name: "errors text inside data", chunks: []string{`{"data":{"message":"contains \"errors\" text"}}`}, wantOutcome: accesscontrol.AuditSucceeded},
		{name: "empty errors list", chunks: []string{`{"data":{},"errors":[]}`}, wantOutcome: accesscontrol.AuditSucceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &controlAuditRecorderStub{}
			handler := controlGraphQLAuditMiddleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, chunk := range test.chunks {
					_, _ = w.Write([]byte(chunk))
				}
			}))
			request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"{ users { total } }"}`))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if len(recorder.events) != 2 || recorder.events[1].Outcome != test.wantOutcome {
				t.Fatalf("events = %#v", recorder.events)
			}
		})
	}
}

func TestControlGraphQLDenialIncludesResolvedContextRequirement(t *testing.T) {
	actor := controlAuditTestActor(t)
	allowedRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionWorkspaceRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	missingRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAccessManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	recorder := &controlAuditRecorderStub{}
	handler := controlGraphQLAuditMiddleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, downstream *http.Request) {
		accesscontrol.CaptureRequiredPermissions(downstream.Context(), []accesscontrol.Requirement{allowedRequirement, missingRequirement})
		accesscontrol.CaptureMissingPermissions(downstream.Context(), []accesscontrol.Requirement{missingRequirement})
		_, _ = w.Write([]byte(`{"errors":[{"message":"forbidden"}]}`))
	}))
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"mutation { createTeam(input: {name: \"Platform\"}) { changed } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	final := recorder.events[1]
	if final.Outcome != accesscontrol.AuditDenied || final.Permission != missingRequirement.Permission || final.Resource != missingRequirement.Resource || len(final.MissingRequirements) != 1 || final.MissingRequirements[0] != missingRequirement {
		t.Fatalf("final denial = %#v", final)
	}
}

type flushTrackingWriter struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (writer *flushTrackingWriter) Flush() {
	writer.flushed = true
	writer.ResponseRecorder.Flush()
}

func TestAuditStatusWriterPreservesFlush(t *testing.T) {
	underlying := &flushTrackingWriter{ResponseRecorder: httptest.NewRecorder()}
	writer := newAuditStatusWriter(underlying, false, true, false)
	flusher, ok := any(writer).(http.Flusher)
	if !ok {
		t.Fatal("audit writer must implement http.Flusher")
	}
	flusher.Flush()
	if !underlying.flushed || !writer.committed || writer.status != http.StatusOK {
		t.Fatalf("flushed/committed/status = %v/%v/%d", underlying.flushed, writer.committed, writer.status)
	}
}

func TestAuditStatusWriterDefersMutationFlushUntilCommit(t *testing.T) {
	underlying := &flushTrackingWriter{ResponseRecorder: httptest.NewRecorder()}
	writer := newAuditStatusWriter(underlying, true, true, true)
	_, _ = writer.Write([]byte(`{"data":{}}`))
	writer.Flush()
	if underlying.flushed || writer.committed {
		t.Fatalf("mutation response flushed before final audit: flushed=%v committed=%v", underlying.flushed, writer.committed)
	}
	writer.commit()
	if !underlying.flushed || !writer.committed || underlying.Body.String() != `{"data":{}}` {
		t.Fatalf("final flush = %v/%v/%q", underlying.flushed, writer.committed, underlying.Body.String())
	}
}

func TestControlAuditOversizedMutationResponsePreservesOriginalResult(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	recorder := &controlAuditRecorderStub{}
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxAuditResponseBytes+1))
		})),
	)
	request := httptest.NewRequest(http.MethodPut, "/account", nil)
	request.Header.Set("X-API-Key", "fsk_license")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() != maxAuditResponseBytes+1 {
		t.Fatalf("oversized mutation response = %d/%d", response.Code, response.Body.Len())
	}
	if len(recorder.events) != 2 || recorder.events[1].Outcome != accesscontrol.AuditSucceeded {
		t.Fatalf("events = %#v", recorder.events)
	}
}

func TestControlAuditFinalizationSurvivesRequestCancellation(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionAccountManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
	recorder := &controlAuditRecorderStub{requireLiveContext: true}
	ctx, cancel := context.WithCancel(context.Background())
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusNoContent)
	}))
	request := requestWithActor(t, http.MethodPut, "/account", actor).WithContext(accesscontrol.ContextWithActor(ctx, actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || len(recorder.events) != 2 || recorder.events[1].Outcome != accesscontrol.AuditSucceeded {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
}

func TestControlAuditPanicPersistsFailureAndRethrows(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionAccountManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
	recorder := &controlAuditRecorderStub{}
	protected := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler failed")
	}))
	response := httptest.NewRecorder()
	middleware.Recoverer(protected).ServeHTTP(response, requestWithActor(t, http.MethodPut, "/account", actor))
	if response.Code != http.StatusInternalServerError || len(recorder.events) != 2 {
		t.Fatalf("status/events = %d/%#v", response.Code, recorder.events)
	}
	final := recorder.events[1]
	if final.Outcome != accesscontrol.AuditFailed || final.ReasonCode != "handler_panic" {
		t.Fatalf("panic audit = %#v", final)
	}
}

type sensitiveReadAuditCase struct {
	name, path   string
	recorder     accesscontrol.AuditRecorder
	stream       bool
	wantStatus   int
	wantExecuted bool
	wantEvents   int
}

func TestSensitiveReadAuditFailsClosedAndStreamingKeepsReceipt(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionBillingRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAccountRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
	)
	tests := []sensitiveReadAuditCase{
		{name: "nil recorder", path: "/credits/pricing", wantStatus: http.StatusServiceUnavailable},
		{name: "preflight failure", path: "/credits/pricing", recorder: &controlAuditRecorderStub{err: errors.New("offline")}, wantStatus: http.StatusServiceUnavailable},
		{name: "final failure", path: "/credits/pricing", recorder: &controlAuditRecorderStub{failFromCall: 2}, wantStatus: http.StatusServiceUnavailable, wantExecuted: true, wantEvents: 1},
		{name: "stream final failure", path: "/account/balance/stream", recorder: &controlAuditRecorderStub{failFromCall: 2}, stream: true, wantStatus: http.StatusOK, wantExecuted: true, wantEvents: 1},
		{name: "account nil recorder", path: "/account", wantStatus: http.StatusServiceUnavailable},
		{name: "account preflight failure", path: "/account", recorder: &controlAuditRecorderStub{err: errors.New("offline")}, wantStatus: http.StatusServiceUnavailable},
		{name: "account final failure", path: "/account", recorder: &controlAuditRecorderStub{failFromCall: 2}, wantStatus: http.StatusServiceUnavailable, wantExecuted: true, wantEvents: 1},
		{name: "account success", path: "/account", recorder: &controlAuditRecorderStub{}, wantStatus: http.StatusOK, wantExecuted: true, wantEvents: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runSensitiveReadAuditCase(t, actor, test)
		})
	}
}

func runSensitiveReadAuditCase(t *testing.T, actor accesscontrol.Actor, test sensitiveReadAuditCase) {
	t.Helper()
	executed := false
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, test.recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executed = true
		_, _ = w.Write([]byte("sensitive"))
		if test.stream {
			w.(http.Flusher).Flush()
		}
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, http.MethodGet, test.path, actor))
	assertSensitiveReadResponse(t, response, executed, test)
	assertSensitiveReadEvents(t, test)
}

func assertSensitiveReadResponse(t *testing.T, response *httptest.ResponseRecorder, executed bool, test sensitiveReadAuditCase) {
	t.Helper()
	if response.Code != test.wantStatus || executed != test.wantExecuted {
		t.Fatalf("status/executed = %d/%v", response.Code, executed)
	}
	if test.stream && response.Body.String() != "sensitive" {
		t.Fatalf("stream body = %q", response.Body.String())
	}
	if !test.stream && test.wantStatus == http.StatusServiceUnavailable && strings.Contains(response.Body.String(), "sensitive") {
		t.Fatalf("sensitive body escaped failed audit: %q", response.Body.String())
	}
}

func assertSensitiveReadEvents(t *testing.T, test sensitiveReadAuditCase) {
	t.Helper()
	recorder, ok := test.recorder.(*controlAuditRecorderStub)
	if !ok {
		return
	}
	if len(recorder.events) != test.wantEvents {
		t.Fatalf("audit events = %#v, want %d", recorder.events, test.wantEvents)
	}
	if test.wantEvents == 2 && (recorder.events[0].Outcome != accesscontrol.AuditAttempted || recorder.events[1].Outcome != accesscontrol.AuditSucceeded) {
		t.Fatalf("account success audit events = %#v", recorder.events)
	}
}

func TestDiscardInboundRequestIDGeneratesInternalID(t *testing.T) {
	var requestID string
	handler := discardInboundRequestID(middleware.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requestID = middleware.GetReqID(r.Context())
	})))
	request := httptest.NewRequest(http.MethodGet, "/account", nil)
	request.Header.Set(middleware.RequestIDHeader, "fsk_secret_that_must_not_be_trusted")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if requestID == "" || strings.HasPrefix(requestID, "fsk_") {
		t.Fatalf("request ID = %q", requestID)
	}
}

func TestWebhookServerUsesBoundedTimeouts(t *testing.T) {
	originalPort := webhookPort
	t.Cleanup(func() { webhookPort = originalPort })
	webhookPort = "9089"
	server := newWebhookHTTPServer(http.NewServeMux())
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatalf("webhook timeouts are not bounded: %#v", server)
	}
}

func TestControlAuditCapturesSensitiveReadStatus(t *testing.T) {
	workspaceID, bucketID, serviceID := uuid.New(), uuid.New(), uuid.New()
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionCredentialsMetadataRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
	)
	path := "/workspace/buckets/" + bucketID.String() + "/services/" + serviceID.String() + "/connect-config"
	for _, statusCode := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			recorder := &controlAuditRecorderStub{}
			handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusCode)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, requestWithActor(t, http.MethodGet, path, actor))
			if len(recorder.events) != 2 || recorder.events[1].StatusCode != statusCode {
				t.Fatalf("events = %#v", recorder.events)
			}
			want := controlAuditOutcome(statusCode)
			if recorder.events[1].Outcome != want {
				t.Fatalf("outcome = %q, want %q", recorder.events[1].Outcome, want)
			}
		})
	}
}

func controlAuditTestActor(t *testing.T) accesscontrol.Actor {
	t.Helper()
	principal := controlTestPrincipal()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(principal.Revision, principal.EffectiveGrants...)
	if err != nil {
		t.Fatal(err)
	}
	return accesscontrol.Actor{
		AccountID: principal.AccountID, WorkspaceID: principal.WorkspaceID,
		SubjectID: principal.SubjectID, CredentialID: principal.CredentialID,
		Kind: principal.Kind, Authorization: snapshot,
	}
}
