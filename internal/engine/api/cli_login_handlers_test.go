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
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/cliauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type cliLoginHandlerFixture struct {
	start       cliauth.StartResult
	approvedID  uuid.UUID
	approvedKey string
	actor       accesscontrol.Actor
	poll        store.CLILoginCredential
	pollErr     error
	logout      cliauth.LogoutInput
	logoutErr   error
}

func (f *cliLoginHandlerFixture) Start(context.Context, cliauth.StartInput) (cliauth.StartResult, error) {
	return f.start, nil
}

func (f *cliLoginHandlerFixture) Approve(_ context.Context, id uuid.UUID, token string, actor accesscontrol.Actor) error {
	f.approvedID, f.approvedKey, f.actor = id, token, actor
	return nil
}

func (f *cliLoginHandlerFixture) Poll(context.Context, uuid.UUID, string) (store.CLILoginCredential, error) {
	return f.poll, f.pollErr
}

func (f *cliLoginHandlerFixture) Logout(_ context.Context, input cliauth.LogoutInput) error {
	f.logout = input
	return f.logoutErr
}

func TestCLILoginStartAndPendingPollArePublicAndNoStore(t *testing.T) {
	transactionID := uuid.New()
	fixture := &cliLoginHandlerFixture{start: cliauth.StartResult{
		TransactionID: transactionID, PollToken: "poll", BrowserToken: "browser",
		ExpiresAt: time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC),
	}, pollErr: store.ErrCLILoginPending}
	router := chi.NewRouter()
	MountCLILoginRoutes(router, fixture, nil)
	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/auth/cli/start", strings.NewReader(`{"credential_hash":"`+strings.Repeat("a", 64)+`","credential_prefix":"fsk_abcd"}`)))
	if start.Code != http.StatusCreated || start.Header().Get("Cache-Control") != "no-store" || !strings.Contains(start.Body.String(), transactionID.String()) {
		t.Fatalf("start response = %d %#v %s", start.Code, start.Header(), start.Body.String())
	}
	poll := httptest.NewRecorder()
	router.ServeHTTP(poll, httptest.NewRequest(http.MethodPost, "/auth/cli/poll", strings.NewReader(`{"transaction_id":"`+transactionID.String()+`","token":"poll"}`)))
	if poll.Code != http.StatusAccepted || !strings.Contains(poll.Body.String(), `"status":"pending"`) {
		t.Fatalf("poll response = %d %s", poll.Code, poll.Body.String())
	}
}

func TestCLILoginApprovalRequiresCookieAndCSRF(t *testing.T) {
	transactionID := uuid.New()
	cliFixture := &cliLoginHandlerFixture{}
	browserFixture := browserSessionHandlerTestFixture(t)
	browserFixture.actor.CredentialSource = "managed_login"
	router := chi.NewRouter()
	MountCLILoginRoutes(router, cliFixture, browserFixture)
	issued := httptest.NewRecorder()
	browserFixture.cookies.SetSession(issued, browserFixture.credential.RawKey, browserFixture.credential.ExpiresAt)
	cookies := issued.Result().Cookies()
	body := `{"transaction_id":"` + transactionID.String() + `","token":"browser-token"}`

	denied := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/cli/approve", strings.NewReader(body))
	request.Header.Set("Origin", "https://example.com")
	request.AddCookie(cookies[0])
	router.ServeHTTP(denied, request)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("approval without CSRF = %d", denied.Code)
	}

	approved := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/auth/cli/approve", strings.NewReader(body))
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set(browserauth.CSRFHeader, cookies[1].Value)
	request.AddCookie(cookies[0])
	request.AddCookie(cookies[1])
	router.ServeHTTP(approved, request)
	if approved.Code != http.StatusNoContent || cliFixture.approvedID != transactionID || cliFixture.approvedKey != "browser-token" {
		t.Fatalf("approval = %d %#v", approved.Code, cliFixture)
	}
}

func TestCLIWhoAmIReturnsAuthenticatedHumanIdentity(t *testing.T) {
	expiresAt := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)
	actor := accesscontrol.Actor{
		AccountID: uuid.New(), WorkspaceID: uuid.New(), SubjectID: uuid.New(),
		DisplayName: "Ada Lovelace", Email: "ada@example.com", CredentialID: uuid.New(),
		CredentialSource: "managed_cli_login", AuthenticationMethod: "google",
		CredentialExpiresAt: &expiresAt, Kind: accesscontrol.SubjectUser,
	}
	router := chi.NewRouter()
	MountCLILoginRoutes(router, &cliLoginHandlerFixture{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("whoami response = %d %#v", response.Code, response.Header())
	}
	var identity cliWhoAmIResponse
	if err := json.Unmarshal(response.Body.Bytes(), &identity); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if !identity.Authenticated || identity.SubjectID != actor.SubjectID || identity.DisplayName != actor.DisplayName || identity.Email != actor.Email || identity.ExpiresAt == nil || !identity.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("whoami identity = %#v", identity)
	}
	if strings.Contains(response.Body.String(), "credential_prefix") {
		t.Fatalf("whoami exposed credential prefix: %s", response.Body.String())
	}
}

func TestCLILogoutUsesOnlyAuthenticatedActor(t *testing.T) {
	actor := accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(),
		CredentialSource: "managed_cli_login", Kind: accesscontrol.SubjectUser,
	}
	fixture := &cliLoginHandlerFixture{}
	router := chi.NewRouter()
	MountCLILoginRoutes(router, fixture, nil)
	request := httptest.NewRequest(http.MethodPost, "/auth/cli/logout", nil)
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout response = %d %s", response.Code, response.Body.String())
	}
	if fixture.logout.Actor.SubjectID != actor.SubjectID || fixture.logout.Actor.CredentialID != actor.CredentialID {
		t.Fatalf("logout input = %#v", fixture.logout)
	}

	unauthenticated := httptest.NewRecorder()
	router.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/auth/cli/logout", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated logout = %d", unauthenticated.Code)
	}
}
