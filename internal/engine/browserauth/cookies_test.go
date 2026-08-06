package browserauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCookieManagerIssuesHostCookiesAndValidatesCredentialBoundCSRF(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	manager := testCookieManager(t)
	recorder := httptest.NewRecorder()
	manager.SetSession(recorder, "fsk_browser_secret", time.Now().Add(time.Hour))
	cookies := recorder.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != "__Host-fused_session" || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("unexpected session cookies: %#v", cookies)
	}
	if cookies[1].Name != "__Host-fused_csrf" || cookies[1].HttpOnly || !cookies[1].Secure {
		t.Fatalf("unexpected CSRF cookie: %#v", cookies[1])
	}

	request := httptest.NewRequest(http.MethodPost, "/workspace", nil)
	request.AddCookie(cookies[0])
	request.AddCookie(cookies[1])
	request.Header.Set(CSRFHeader, cookies[1].Value)
	if !manager.ValidateCSRF(request, cookies[0].Value) {
		t.Fatal("valid credential-bound CSRF token was rejected")
	}
	if manager.ValidateCSRF(request, "fsk_different_secret") {
		t.Fatal("CSRF token was accepted for a different credential")
	}
}

func TestCredentialFromRequestRejectsHeaderCookieAmbiguity(t *testing.T) {
	manager := testCookieManager(t)
	request := httptest.NewRequest(http.MethodGet, "/workspace", nil)
	request.Header.Set("X-API-Key", "fsk_header")
	request.AddCookie(&http.Cookie{Name: manager.sessionName(), Value: "fsk_cookie"})
	if _, _, err := CredentialFromRequest(request, manager); err != ErrAmbiguousCredential {
		t.Fatalf("ambiguous credential error = %v", err)
	}
}

func TestCredentialFromRequestRejectsDuplicateSessionCookies(t *testing.T) {
	manager := testCookieManager(t)
	request := httptest.NewRequest(http.MethodGet, "/workspace", nil)
	request.AddCookie(&http.Cookie{Name: manager.sessionName(), Value: "fsk_first"})
	request.AddCookie(&http.Cookie{Name: manager.sessionName(), Value: "fsk_second"})
	if _, _, err := CredentialFromRequest(request, manager); err != ErrAmbiguousCredential {
		t.Fatalf("duplicate session cookie error = %v", err)
	}
}

func TestCookieManagerBindsManagedPollToOriginAndInitiatingBrowser(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	manager := testCookieManager(t)
	recorder := httptest.NewRecorder()
	manager.SetLoginBinding(recorder, "transaction", "poll-secret", time.Now().Add(time.Minute))
	binding := recorder.Result().Cookies()[0]
	request := httptest.NewRequest(http.MethodPost, "https://engine.test/auth/managed/poll", nil)
	request.Header.Set("Origin", "https://engine.test")
	request.AddCookie(binding)
	if !manager.ValidateSameOrigin(request) || !manager.ValidateLoginBinding(request, "transaction", "poll-secret") {
		t.Fatal("valid managed-login browser binding was rejected")
	}
	request.Header.Set("Origin", "https://hostile.test")
	if manager.ValidateSameOrigin(request) {
		t.Fatal("cross-origin session-setting request was accepted")
	}
	if manager.ValidateLoginBinding(request, "transaction", "different-secret") {
		t.Fatal("managed-login binding accepted the wrong poll secret")
	}
}

func testCookieManager(t *testing.T) *CookieManager {
	t.Helper()
	manager, err := NewCookieManager([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("NewCookieManager: %v", err)
	}
	return manager
}
