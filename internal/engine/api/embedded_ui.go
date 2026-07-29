package api

import (
	"net/http"
	"path"
	"strings"
	"time"
)

const embeddedUIIndex = "index.html"

// EmbeddedUIMiddleware serves the embedded SPA without taking over Engine API
// routes. Real build artifacts are served first because service workers,
// logos, and other root-level files do not live under /assets.
func EmbeddedUIMiddleware(uiFS http.FileSystem) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if uiFS == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isUIReadMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if serveEmbeddedUIFile(w, r, uiFS, embeddedUIAssetPath(r.URL.Path), "asset") {
				return
			}
			if isEmbeddedUINavigation(r) && serveEmbeddedUIFile(w, r, uiFS, embeddedUIIndex, "spa_fallback") {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isUIReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func isEmbeddedUINavigation(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	// Browser navigations do not include the Engine API key header, while UI
	// fetches do. Keeping that distinction lets paths like /integrations serve
	// the SPA in a tab and still proxy as JSON from the app.
	return r.Header.Get("X-API-Key") == "" && strings.Contains(accept, "text/html")
}

func embeddedUIAssetPath(rawPath string) string {
	return strings.TrimPrefix(path.Clean("/"+rawPath), "/")
}

func serveEmbeddedUIFile(w http.ResponseWriter, r *http.Request, uiFS http.FileSystem, name string, mode string) bool {
	if name == "" || name == "." {
		return false
	}
	start := time.Now()
	openStart := time.Now()
	file, err := uiFS.Open(name)
	openDur := time.Since(openStart)
	if err != nil {
		return false
	}
	defer file.Close()

	statStart := time.Now()
	info, err := file.Stat()
	statDur := time.Since(statStart)
	if err != nil || info.IsDir() {
		return false
	}

	setEmbeddedUICacheHeaders(w, name)
	setEmbeddedUIServerTiming(w.Header(), embeddedUITiming{
		open:  openDur,
		stat:  statDur,
		ready: time.Since(start),
	})

	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return true
}

type embeddedUITiming struct {
	open  time.Duration
	stat  time.Duration
	ready time.Duration
}

func setEmbeddedUIServerTiming(header http.Header, timing embeddedUITiming) {
	header.Set("Server-Timing", strings.Join([]string{
		serverTimingMetric("embedded_ui_open", timing.open),
		serverTimingMetric("embedded_ui_stat", timing.stat),
		serverTimingMetric("embedded_ui_ready", timing.ready),
	}, ", "))
}

func setEmbeddedUICacheHeaders(w http.ResponseWriter, name string) {
	if name == embeddedUIIndex || name == "notification-service-worker.js" {
		// The shell and service worker choose which hashed assets to load, so
		// they must revalidate when a new Engine binary ships a new UI build.
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
}
