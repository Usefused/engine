package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/managedauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type managedLoginHandlerFixture struct {
	startResult managedauth.StartResult
	credential  store.ManagedSessionCredential
	pollErr     error
	pollID      uuid.UUID
	pollToken   string
}

func (f *managedLoginHandlerFixture) Start(context.Context) (managedauth.StartResult, error) {
	return f.startResult, nil
}

func (f *managedLoginHandlerFixture) Poll(_ context.Context, id uuid.UUID, token string) (store.ManagedSessionCredential, error) {
	f.pollID, f.pollToken = id, token
	return f.credential, f.pollErr
}

func TestManagedLoginStartReturnsOnlyBrowserPollingMaterial(t *testing.T) {
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	fixture := &managedLoginHandlerFixture{startResult: managedauth.StartResult{
		TransactionID: uuid.New(), PollToken: "browser-poll-token",
		VerificationURL: "https://auth.usefused.test/login", ExpiresAt: expiresAt,
	}}
	router := chi.NewRouter()
	cookies := managedLoginTestCookies(t)
	MountManagedIdentityRoutes(router, fixture, cookies)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/managed/start", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://example.com")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start status/cache = %d/%q", response.Code, response.Header().Get("Cache-Control"))
	}
	rawBody := response.Body.String()
	var body map[string]string
	if err := json.NewDecoder(strings.NewReader(rawBody)).Decode(&body); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if body["transaction_id"] != fixture.startResult.TransactionID.String() || body["poll_token"] != fixture.startResult.PollToken {
		t.Fatalf("unexpected start response: %#v", body)
	}
	bindingCookies := response.Result().Cookies()
	if strings.Contains(rawBody, "engine_verifier") || len(bindingCookies) != 1 || !bindingCookies[0].HttpOnly || strings.Contains(bindingCookies[0].Value, fixture.startResult.PollToken) {
		t.Fatal("start response exposed server-only state or omitted browser binding")
	}
}

func TestManagedLoginStartDoesNotRequireEnterpriseSSOEntitlement(t *testing.T) {
	// Hosted email and social login share this start path with enterprise SSO,
	// so gating here would incorrectly deny every non-enterprise user before
	// Logto can offer the authentication methods available to that tenant.
	fixture := &managedLoginHandlerFixture{startResult: managedauth.StartResult{
		TransactionID: uuid.New(), PollToken: "browser-poll-token",
		VerificationURL: "https://auth.usefused.test/login", ExpiresAt: time.Now().Add(time.Minute),
	}}
	router := chi.NewRouter()
	MountManagedIdentityRoutes(router, fixture, managedLoginTestCookies(t))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/managed/start", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://example.com")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("non-enterprise managed login status = %d: %s", response.Code, response.Body.String())
	}
}

func TestManagedLoginPollSetsHttpOnlyHostCookieWithoutReturningCredential(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	id := uuid.New()
	fixture := &managedLoginHandlerFixture{credential: store.ManagedSessionCredential{
		RawKey: "fsk_managed_browser_secret", ExpiresAt: time.Now().UTC().Add(time.Hour),
	}}
	router := chi.NewRouter()
	manager := managedLoginTestCookies(t)
	MountManagedIdentityRoutes(router, fixture, manager)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/managed/poll", strings.NewReader(
		`{"transaction_id":"`+id.String()+`","poll_token":"opaque-poll-token"}`,
	))
	request.Header.Set("Origin", "https://example.com")
	binding := httptest.NewRecorder()
	manager.SetLoginBinding(binding, id.String(), "opaque-poll-token", time.Now().Add(time.Minute))
	request.AddCookie(binding.Result().Cookies()[0])
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || fixture.pollID != id || fixture.pollToken != "opaque-poll-token" {
		t.Fatalf("poll result = %d/%s/%q", response.Code, fixture.pollID, fixture.pollToken)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 3 || cookies[0].Name != "__Host-fused_session" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected managed session cookie: %#v", cookies)
	}
	if cookies[1].Name != "__Host-fused_csrf" || cookies[1].HttpOnly || !cookies[1].Secure {
		t.Fatalf("unexpected managed CSRF cookie: %#v", cookies[1])
	}
	if cookies[2].Name != "__Host-fused_login" || cookies[2].MaxAge != -1 {
		t.Fatalf("managed login binding was not cleared: %#v", cookies[2])
	}
	if cookies[0].Value != fixture.credential.RawKey || strings.Contains(response.Body.String(), fixture.credential.RawKey) {
		t.Fatal("managed credential was not confined to the HttpOnly cookie")
	}
}

func TestManagedLoginPollPendingAndDisabledAreBounded(t *testing.T) {
	id := uuid.New()
	fixture := &managedLoginHandlerFixture{pollErr: store.ErrManagedLoginPending}
	router := chi.NewRouter()
	manager := managedLoginTestCookies(t)
	MountManagedIdentityRoutes(router, fixture, manager)
	requestBody := `{"transaction_id":"` + id.String() + `","poll_token":"opaque-poll-token"}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/managed/poll", strings.NewReader(requestBody))
	request.Header.Set("Origin", "https://example.com")
	binding := httptest.NewRecorder()
	manager.SetLoginBinding(binding, id.String(), "opaque-poll-token", time.Now().Add(time.Minute))
	request.AddCookie(binding.Result().Cookies()[0])
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending response = %d %s", response.Code, response.Body.String())
	}

	disabled := chi.NewRouter()
	MountManagedIdentityRoutes(disabled, nil, managedLoginTestCookies(t))
	response = httptest.NewRecorder()
	disabled.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/auth/managed/start", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "managed_login_unavailable") {
		t.Fatalf("disabled response = %d %s", response.Code, response.Body.String())
	}
}

func TestManagedLoginRejectsCrossOriginMissingBindingAndRateLimits(t *testing.T) {
	id := uuid.New()
	fixture := &managedLoginHandlerFixture{
		startResult: managedauth.StartResult{
			TransactionID: id, PollToken: "opaque-poll-token",
			VerificationURL: "https://auth.usefused.test/login", ExpiresAt: time.Now().Add(time.Minute),
		},
		pollErr: store.ErrManagedLoginPending,
	}
	manager := managedLoginTestCookies(t)
	router := chi.NewRouter()
	MountManagedIdentityRoutes(router, fixture, manager)

	crossOrigin := httptest.NewRequest(http.MethodPost, "/auth/managed/start", strings.NewReader(`{}`))
	crossOrigin.Header.Set("Origin", "https://hostile.test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, crossOrigin)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin start status = %d", response.Code)
	}

	pollBody := `{"transaction_id":"` + id.String() + `","poll_token":"opaque-poll-token"}`
	missingBinding := httptest.NewRequest(http.MethodPost, "/auth/managed/poll", strings.NewReader(pollBody))
	missingBinding.Header.Set("Origin", "https://example.com")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, missingBinding)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing-binding poll status = %d", response.Code)
	}

	startRouter := chi.NewRouter()
	MountManagedIdentityRoutes(startRouter, fixture, manager)
	for index := 0; index < 11; index++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/managed/start", strings.NewReader(`{}`))
		request.Header.Set("Origin", "https://example.com")
		response = httptest.NewRecorder()
		startRouter.ServeHTTP(response, request)
	}
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("managed start rate limit status = %d", response.Code)
	}

	pollRouter := chi.NewRouter()
	MountManagedIdentityRoutes(pollRouter, fixture, manager)
	binding := httptest.NewRecorder()
	manager.SetLoginBinding(binding, id.String(), "opaque-poll-token", time.Now().Add(time.Minute))
	for index := 0; index < 121; index++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/managed/poll", strings.NewReader(pollBody))
		request.Header.Set("Origin", "https://example.com")
		request.AddCookie(binding.Result().Cookies()[0])
		response = httptest.NewRecorder()
		pollRouter.ServeHTTP(response, request)
	}
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("managed poll rate limit status = %d", response.Code)
	}
}

func managedLoginTestCookies(t *testing.T) *browserauth.CookieManager {
	t.Helper()
	manager, err := browserauth.NewCookieManager([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("NewCookieManager: %v", err)
	}
	return manager
}
