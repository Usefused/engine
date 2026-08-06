package api

import (
	"context"
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
