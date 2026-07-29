package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestEmbeddedUIMiddleware(t *testing.T) {
	handler := testEmbeddedUIHandler()

	tests := []struct {
		name       string
		method     string
		path       string
		accept     string
		apiKey     string
		wantStatus int
		wantBody   string
		wantCache  string
	}{
		{
			name:       "serves spa for browser route",
			method:     http.MethodGet,
			path:       "/integrations/stripe",
			accept:     "text/html,application/xhtml+xml",
			wantStatus: http.StatusOK,
			wantBody:   "<html>spa</html>",
			wantCache:  "no-cache",
		},
		{
			name:       "serves root static asset",
			method:     http.MethodGet,
			path:       "/logo.svg",
			accept:     "image/svg+xml",
			wantStatus: http.StatusOK,
			wantBody:   "<svg></svg>",
		},
		{
			name:       "serves service worker asset",
			method:     http.MethodGet,
			path:       "/notification-service-worker.js",
			accept:     "*/*",
			wantStatus: http.StatusOK,
			wantBody:   "self.addEventListener('push', () => {})",
			wantCache:  "no-cache",
		},
		{
			name:       "serves hashed asset",
			method:     http.MethodHead,
			path:       "/assets/app.js",
			accept:     "*/*",
			wantStatus: http.StatusOK,
			wantCache:  "public, max-age=31536000, immutable",
		},
		{
			name:       "passes authenticated api request through",
			method:     http.MethodGet,
			path:       "/integrations",
			accept:     "text/html",
			apiKey:     "test-key",
			wantStatus: http.StatusTeapot,
			wantBody:   "next",
		},
		{
			name:       "passes writes through",
			method:     http.MethodPost,
			path:       "/integrations",
			accept:     "text/html",
			wantStatus: http.StatusTeapot,
			wantBody:   "next",
		},
		{
			name:       "passes missing non navigation asset through",
			method:     http.MethodGet,
			path:       "/missing.js",
			accept:     "*/*",
			wantStatus: http.StatusTeapot,
			wantBody:   "next",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Accept", tt.accept)
			if tt.apiKey != "" {
				req.Header.Set("X-API-Key", tt.apiKey)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantCache != "" && rec.Header().Get("Cache-Control") != tt.wantCache {
				t.Fatalf("cache-control = %q, want %q", rec.Header().Get("Cache-Control"), tt.wantCache)
			}
			if tt.wantStatus == http.StatusOK {
				assertEmbeddedUIServerTiming(t, rec.Header().Get("Server-Timing"))
			} else if rec.Header().Get("Server-Timing") != "" {
				t.Fatalf("Server-Timing = %q, want empty for pass-through", rec.Header().Get("Server-Timing"))
			}
		})
	}
}

func TestEmbeddedUIMiddlewareNilFilesystemPassesThrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("next"))
	})
	handler := EmbeddedUIMiddleware(nil)(next)
	req := httptest.NewRequest(http.MethodGet, "/integrations", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "next" {
		t.Fatalf("body = %q, want next", rec.Body.String())
	}
}

func assertEmbeddedUIServerTiming(t *testing.T, timing string) {
	t.Helper()
	for _, want := range []string{"embedded_ui_open;dur=", "embedded_ui_stat;dur=", "embedded_ui_ready;dur="} {
		if !strings.Contains(timing, want) {
			t.Fatalf("Server-Timing = %q, want metric %q", timing, want)
		}
	}
}

func testEmbeddedUIHandler() http.Handler {
	modTime := time.Unix(1700000000, 0)
	uiFS := http.FS(fstest.MapFS{
		"index.html": {
			Data:    []byte("<html>spa</html>"),
			Mode:    0o644,
			ModTime: modTime,
		},
		"logo.svg": {
			Data:    []byte("<svg></svg>"),
			Mode:    0o644,
			ModTime: modTime,
		},
		"notification-service-worker.js": {
			Data:    []byte("self.addEventListener('push', () => {})"),
			Mode:    0o644,
			ModTime: modTime,
		},
		"assets/app.js": {
			Data:    []byte("console.log('ok')"),
			Mode:    0o644,
			ModTime: modTime,
		},
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("next"))
	})
	return EmbeddedUIMiddleware(uiFS)(next)
}
