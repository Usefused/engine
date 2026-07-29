package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newCORSTestHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestCORS_SingleOrigin_Allowed covers the Engine's existing call style
// (CORS(cfg.UIURL), one string, no comma) -- must keep working unchanged
// since this is the only origin the Engine's UI ever calls from.
func TestCORS_SingleOrigin_Allowed(t *testing.T) {
	h := CORS("https://app.usefused.com")(newCORSTestHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.usefused.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.usefused.com" {
		t.Errorf("expected Access-Control-Allow-Origin %q, got %q", "https://app.usefused.com", got)
	}
}

// TestCORS_MultipleOrigins_BothAllowed covers the Registry's new call style
// after Sprint 3: CORS(cfg.UIURL, cfg.HomepageURL). The homepage and the app
// are two separate deployments on two separate origins, and the Registry
// needs to accept browser calls from either -- not just the first one.
func TestCORS_MultipleOrigins_BothAllowed(t *testing.T) {
	h := CORS("https://app.usefused.com", "https://usefused.com")(newCORSTestHandler())

	for _, origin := range []string{"https://app.usefused.com", "https://usefused.com"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %s: expected Access-Control-Allow-Origin %q, got %q", origin, origin, got)
		}
	}
}

// TestCORS_UnknownOrigin_FallsBackToDefault covers a request from an origin
// that isn't in the allowlist -- it must not be echoed back (that would be
// an open reflection of any Origin header), falling back to the first
// configured origin instead.
func TestCORS_UnknownOrigin_FallsBackToDefault(t *testing.T) {
	h := CORS("https://app.usefused.com", "https://usefused.com")(newCORSTestHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.usefused.com" {
		t.Errorf("expected unknown origin to fall back to the first configured origin, got %q", got)
	}
}

// TestCORS_FirstOriginEmpty_SecondStillUsedAsDefault guards a real bug fixed
// while adding multi-origin support: the original implementation picked the
// default origin by array index (originsList[0]) rather than the first
// non-empty entry, so a leading empty/unset origin (e.g. an unset
// HomepageURL passed before UIURL, or a stray leading comma) silently fell
// through to "*" even though a perfectly good origin was configured right
// after it.
func TestCORS_FirstOriginEmpty_SecondStillUsedAsDefault(t *testing.T) {
	h := CORS("", "https://app.usefused.com")(newCORSTestHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://someone-else.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.usefused.com" {
		t.Errorf("expected the first non-empty origin to be used as default, got %q", got)
	}
}

// TestCORS_NoOriginsConfigured_Wildcard preserves the existing fallback:
// when nothing is configured at all, allow any origin (matches local dev's
// current behavior, where UIURL sometimes isn't set).
func TestCORS_NoOriginsConfigured_Wildcard(t *testing.T) {
	h := CORS("")(newCORSTestHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://anywhere.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("expected wildcard fallback, got %q", got)
	}
}

// TestCORS_OPTIONSPreflight_Returns204NoContent preserves existing preflight
// handling -- must respond before reaching the wrapped handler.
func TestCORS_OPTIONSPreflight_Returns204NoContent(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := CORS("https://app.usefused.com")(next)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	if called {
		t.Error("preflight OPTIONS request must not reach the wrapped handler")
	}
}
