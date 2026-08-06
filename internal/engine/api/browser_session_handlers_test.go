package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type browserSessionHandlerFixture struct {
	cookies    *browserauth.CookieManager
	credential store.BrowserSessionCredential
	actor      accesscontrol.Actor
	logoutKey  string
	logoutErr  error
}

func (f *browserSessionHandlerFixture) ExchangeLicenseKey(context.Context, string) (store.BrowserSessionCredential, error) {
	return f.credential, nil
}

func (f *browserSessionHandlerFixture) Session(context.Context, string) (accesscontrol.Actor, error) {
	return f.actor, nil
}

func (f *browserSessionHandlerFixture) Logout(_ context.Context, raw string) error {
	f.logoutKey = raw
	return f.logoutErr
}

func TestBrowserLogoutKeepsSessionCookieOnPersistenceFailure(t *testing.T) {
	fixture := browserSessionHandlerTestFixture(t)
	fixture.logoutErr = errors.New("database unavailable")
	router := chi.NewRouter()
	MountBrowserSessionRoutes(router, fixture)
	issued := httptest.NewRecorder()
	fixture.cookies.SetSession(issued, fixture.credential.RawKey, fixture.credential.ExpiresAt)
	cookies := issued.Result().Cookies()
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(`{}`))
	request.AddCookie(cookies[0])
	request.AddCookie(cookies[1])
	request.Header.Set(browserauth.CSRFHeader, cookies[1].Value)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("failed logout status/cookie = %d/%q", response.Code, response.Header().Get("Set-Cookie"))
	}
}

func (f *browserSessionHandlerFixture) Cookies() *browserauth.CookieManager { return f.cookies }

func TestBrowserLicenseExchangeConfinesDerivedCredentialToCookies(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	fixture := browserSessionHandlerTestFixture(t)
	router := chi.NewRouter()
	MountBrowserSessionRoutes(router, fixture)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/license/exchange", strings.NewReader(`{"license_key":"license-secret"}`))
	request.Header.Set("Origin", "https://example.com")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), fixture.credential.RawKey) || strings.Contains(response.Body.String(), "license-secret") {
		t.Fatalf("license exchange response = %d %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Value != fixture.credential.RawKey || !cookies[0].HttpOnly || cookies[1].HttpOnly {
		t.Fatalf("unexpected browser session cookies: %#v", cookies)
	}
}

func TestBrowserLicenseExchangeRejectsCrossOriginAndRateLimits(t *testing.T) {
	fixture := browserSessionHandlerTestFixture(t)
	router := chi.NewRouter()
	MountBrowserSessionRoutes(router, fixture)
	request := httptest.NewRequest(http.MethodPost, "/auth/license/exchange", strings.NewReader(`{"license_key":"license-secret"}`))
	request.Header.Set("Origin", "https://hostile.test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin exchange status = %d", response.Code)
	}
	for index := 0; index < 10; index++ {
		request = httptest.NewRequest(http.MethodPost, "/auth/license/exchange", strings.NewReader(`{"license_key":"license-secret"}`))
		request.Header.Set("Origin", "https://example.com")
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
	}
	request = httptest.NewRequest(http.MethodPost, "/auth/license/exchange", strings.NewReader(`{"license_key":"license-secret"}`))
	request.Header.Set("Origin", "https://example.com")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited exchange status = %d", response.Code)
	}
}

func TestBrowserSessionStatusAndLogoutRequireCookieBoundCSRF(t *testing.T) {
	fixture := browserSessionHandlerTestFixture(t)
	router := chi.NewRouter()
	MountBrowserSessionRoutes(router, fixture)
	cookieResponse := httptest.NewRecorder()
	fixture.cookies.SetSession(cookieResponse, fixture.credential.RawKey, fixture.credential.ExpiresAt)
	cookies := cookieResponse.Result().Cookies()

	statusRequest := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	statusRequest.AddCookie(cookies[0])
	statusResponse := httptest.NewRecorder()
	router.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"authenticated":true`) {
		t.Fatalf("session response = %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(`{}`))
	logoutRequest.AddCookie(cookies[0])
	logoutRequest.AddCookie(cookies[1])
	logoutResponse := httptest.NewRecorder()
	router.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusForbidden || fixture.logoutKey != "" {
		t.Fatalf("logout without CSRF = %d key=%q", logoutResponse.Code, fixture.logoutKey)
	}

	logoutRequest = httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(`{}`))
	logoutRequest.AddCookie(cookies[0])
	logoutRequest.AddCookie(cookies[1])
	logoutRequest.Header.Set(browserauth.CSRFHeader, cookies[1].Value)
	logoutResponse = httptest.NewRecorder()
	router.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusNoContent || fixture.logoutKey != fixture.credential.RawKey {
		t.Fatalf("logout response = %d key=%q", logoutResponse.Code, fixture.logoutKey)
	}
}

func browserSessionHandlerTestFixture(t *testing.T) *browserSessionHandlerFixture {
	t.Helper()
	cookies, err := browserauth.NewCookieManager([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("NewCookieManager: %v", err)
	}
	return &browserSessionHandlerFixture{
		cookies: cookies,
		credential: store.BrowserSessionCredential{
			SubjectID: uuid.New(), CredentialID: uuid.New(), RawKey: "fsk_browser_derived",
			ExpiresAt: time.Now().Add(time.Hour), AuthorizationRevision: 8,
		},
		actor: accesscontrol.Actor{SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectBootstrap},
	}
}

var _ BrowserSessionService = (*browserSessionHandlerFixture)(nil)
