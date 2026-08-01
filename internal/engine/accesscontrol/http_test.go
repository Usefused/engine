package accesscontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	if body.Missing[0].Permission != requirement.Permission || body.Missing[0].ResourceID != requirement.Resource.ID.String() {
		t.Fatalf("missing requirement = %#v", body.Missing[0])
	}
}

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
	if len(body.Missing) != 1 || body.Missing[0].DisplayName != "payments-production" {
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
	if body.Error != code || len(body.Missing) != missing {
		t.Fatalf("response = %#v, want error=%q missing=%d", body, code, missing)
	}
}
