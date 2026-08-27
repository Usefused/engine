package accesscontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

func TestRequireAllReturns401WithoutActor(t *testing.T) {
	handler := RequireAll(SnapshotAuthorizer{}, testWorkspaceRequirement())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler must not execute")
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
	assertDenialResponse(t, response, http.StatusUnauthorized, "authentication_required", 0)
}

func TestWriteAuthorizationErrorReturnsStablePolicyDenial(t *testing.T) {
	response := httptest.NewRecorder()
	WriteAuthorizationError(response, ErrPolicyDenied)
	assertDenialResponse(t, response, http.StatusForbidden, "permission_denied", 0)
}

// TestWriteAuthorizationErrorIncludesRequestCorrelation proves middleware identity is mirrored in the envelope and header.
func TestWriteAuthorizationErrorIncludesRequestCorrelation(t *testing.T) {
	handler := chimiddleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteAuthorizationError(w, ErrAuthenticationRequired, r.Context())
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
	var body denialResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// The generated request ID must be identical in both correlation surfaces.
	if body.Error.RequestID == "" || response.Header().Get("X-Request-ID") != body.Error.RequestID {
		t.Fatalf("request correlation = body %q header %q", body.Error.RequestID, response.Header().Get("X-Request-ID"))
	}
}

// TestRequireAllReturns403WithSafeMissingRequirements proves typed policy details stay nested and bounded.
func TestRequireAllReturns403WithSafeMissingRequirements(t *testing.T) {
	requirement := testWorkspaceRequirement()
	actor := Actor{SubjectID: uuid.New(), Authorization: mustSnapshot(t)}
	handler := RequireAll(SnapshotAuthorizer{}, requirement)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler must not execute")
	}))
	request := httptest.NewRequest(http.MethodPost, "/protected", nil)
	request = request.WithContext(ContextWithActor(request.Context(), actor))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertDenialResponse(t, response, http.StatusForbidden, "permission_denied", 1)

	var body denialResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Permission details remain nested under the shared error envelope.
	if body.Error.Details == nil || body.Error.Details.Missing[0].Permission != requirement.Permission || body.Error.Details.Missing[0].ResourceID != requirement.Resource.ID.String() {
		t.Fatalf("missing requirement = %#v", body.Error.Details)
	}
	// Authorization stopped this unsafe request before its protected mutation began.
	if body.Error.Phase != "authorization" || body.Error.CommitState != "not_committed" {
		t.Fatalf("mutation denial metadata = %#v", body.Error)
	}
}

// TestWriteAuthorizationErrorIncludesOnlyProvidedSafeDisplayName proves only reviewed labels reach the envelope.
func TestWriteAuthorizationErrorIncludesOnlyProvidedSafeDisplayName(t *testing.T) {
	resource := ResourceRef{Type: ResourceBucket, ID: uuid.New()}
	response := httptest.NewRecorder()
	WriteAuthorizationError(response, &PermissionDeniedError{
		Missing:      []Requirement{{Permission: PermissionBucketUse, Resource: resource}},
		DisplayNames: map[ResourceRef]string{resource: "payments-production"},
	})
	var body denialResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// The reviewed display name is the only resource label exposed to the caller.
	if body.Error.Details == nil || len(body.Error.Details.Missing) != 1 || body.Error.Details.Missing[0].DisplayName != "payments-production" {
		t.Fatalf("response = %#v", body)
	}
}

func TestRequireAllCallsHandlerWhenEveryRequirementIsAllowed(t *testing.T) {
	requirement := testWorkspaceRequirement()
	snapshot := mustSnapshot(t, Grant(requirement))
	actor := Actor{SubjectID: uuid.New(), Authorization: snapshot}
	called := false
	handler := RequireAll(SnapshotAuthorizer{}, requirement)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/protected", nil)
	request = request.WithContext(ContextWithActor(request.Context(), actor))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called/status = %v/%d, want true/%d", called, response.Code, http.StatusNoContent)
	}
	if !strings.Contains(response.Header().Get("Server-Timing"), "engine_authz;dur=") {
		t.Fatalf("Server-Timing = %q", response.Header().Get("Server-Timing"))
	}
}

func TestActorContextRoundTrip(t *testing.T) {
	want := Actor{SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: SubjectUser}
	got, ok := ActorFromContext(ContextWithActor(context.Background(), want))
	if !ok || got.SubjectID != want.SubjectID || got.CredentialID != want.CredentialID || got.Kind != want.Kind {
		t.Fatalf("ActorFromContext = %#v, %v; want %#v, true", got, ok, want)
	}
	if _, ok := ActorFromContext(context.Background()); ok {
		t.Fatal("ActorFromContext unexpectedly found actor")
	}
}

func testWorkspaceRequirement() Requirement {
	return Requirement{
		Permission: PermissionWorkspaceUpdate,
		Resource:   ResourceRef{Type: ResourceWorkspace, ID: uuid.New()},
	}
}

// assertDenialResponse checks the common authorization envelope and optional typed details.
func assertDenialResponse(t *testing.T, response *httptest.ResponseRecorder, status int, code string, missing int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	var body denialResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	missingCount := 0
	// An omitted details object represents a denial with no safe field diagnostics.
	if body.Error.Details != nil {
		missingCount = len(body.Error.Details.Missing)
	}
	if body.Error.Code != code || missingCount != missing {
		t.Fatalf("response = %#v, want error=%q missing=%d", body, code, missing)
	}
}
