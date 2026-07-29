package middleware

import (
	"net/http"
	"strings"
)

// CORS allows browser clients to call the API. Each argument is an allowed
// origin, or a comma-separated list of them (the original call style, still
// used by the Engine: CORS(cfg.UIURL), one origin, its own UI). Variadic so
// the Registry can pass multiple distinct origins directly -- since Sprint 3
// split the homepage into its own deployment, the Registry now needs to
// accept browser calls from both the app's origin and the homepage's origin,
// not just one. Passing both here (an explicit allowlist) is the point: the
// alternative of reaching for a wildcard to unblock a second origin would
// open every route behind this middleware to any origin, not just the two
// that actually need it.
func CORS(allowedOrigins ...string) func(http.Handler) http.Handler {
	originsList := strings.Split(strings.Join(allowedOrigins, ","), ",")
	allowedMap := make(map[string]bool)
	var defaultOrigin string
	for _, o := range originsList {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		// First non-empty origin wins, not literally the first array
		// index -- a leading empty/unset origin (e.g. an unset second
		// origin passed before a configured one) must not shadow a real
		// one that comes after it.
		if defaultOrigin == "" {
			defaultOrigin = o
		}
		allowedMap[o] = true
	}
	if defaultOrigin == "" {
		defaultOrigin = "*"
		allowedMap["*"] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqOrigin := r.Header.Get("Origin")
			originToSet := defaultOrigin

			if allowedMap["*"] {
				originToSet = "*"
			} else if reqOrigin != "" && allowedMap[reqOrigin] {
				originToSet = reqOrigin
			}

			w.Header().Set("Access-Control-Allow-Origin", originToSet)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, x-api-key")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
