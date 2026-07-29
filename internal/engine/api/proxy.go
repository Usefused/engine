// Package api hosts the Engine's HTTP surface: handlers that proxy
// Registry-bound traffic and handlers that serve Engine-local data.
package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// RegistryProxy forwards HTTP requests from the Engine to the Registry.
// It is the single place reverse-proxy behavior (Host rewriting, path
// handling) lives, so GraphQL and REST proxy handlers don't each reimplement
// it -- that's the DRY boundary this file exists to enforce.
type RegistryProxy struct {
	target *url.URL
}

// NewRegistryProxy builds a proxy targeting the Registry's base URL.
// registryEndpoint is accepted in either form the config already uses
// elsewhere in the codebase: a bare host or the GraphQL-suffixed form used by
// the Engine's metadata client in registry_client.go. The suffix is
// stripped so one proxy instance can serve both /graphql and REST paths --
// mirroring the same trailing-suffix trim registry_client.go already does
// when it derives the handshake URL from the same config value.
func NewRegistryProxy(registryEndpoint string) *RegistryProxy {
	base := strings.TrimSuffix(registryEndpoint, "/graphql")

	target, err := url.Parse(base)
	if err != nil || target.Scheme == "" || target.Host == "" {
		// Defer the failure to request time (every Forward call will error)
		// rather than panicking at construction. A misconfigured endpoint
		// should show up as failed requests in logs/OTEL, not a boot crash --
		// the Engine's other subsystems shouldn't go down over this.
		slog.Error("RegistryProxy: invalid registry endpoint, proxy will fail all requests",
			slog.String("registry_endpoint", registryEndpoint), slog.Any("error", err))
		target = &url.URL{}
	}

	return &RegistryProxy{target: target}
}

// Forward relays r to the Registry and copies the Registry's response
// (status, headers, body) back to w unchanged. stripPrefix, if non-empty, is
// removed from the outgoing request path before forwarding.
//
// The caller's original X-API-Key header is left untouched: the Registry
// owns identity resolution for its own endpoints, and Engine-side key
// validation (done by callers of Forward, before they call it) only gates
// whether the request reaches this point -- it doesn't replace the
// Registry's own auth check.
func (p *RegistryProxy) Forward(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	proxy := p.newReverseProxy(stripPrefix)
	proxy.ModifyResponse = func(res *http.Response) error {
		stripCORSHeaders(res)
		return nil
	}
	proxy.ServeHTTP(w, r)
}

// ForwardAndInspect behaves exactly like Forward (path stripping, Host
// rewriting, CORS header removal, byte-for-byte body passthrough to the
// client), but additionally invokes onSuccess with the buffered response
// body whenever the Registry answered with a 2xx status. This is how the
// import/apply auto-register intercept (engine_workspace_registration_plan.md,
// Task 3) reads what was just applied without altering what the client
// ultimately receives -- onSuccess runs against a copy of the bytes, and the
// response body is restored before ServeHTTP writes it out.
//
// A non-2xx response never invokes onSuccess -- there's nothing to
// auto-register from a failed apply -- but the body still passes through to
// the client unchanged either way.
func (p *RegistryProxy) ForwardAndInspect(w http.ResponseWriter, r *http.Request, stripPrefix string, onSuccess func(body []byte)) {
	proxy := p.newReverseProxy(stripPrefix)
	proxy.ModifyResponse = func(res *http.Response) error {
		stripCORSHeaders(res)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(body))
		if onSuccess != nil {
			onSuccess(body)
		}
		return nil
	}
	proxy.ServeHTTP(w, r)
}

// newReverseProxy builds the *httputil.ReverseProxy shared Director (path
// stripping, Host rewriting) both Forward and ForwardAndInspect need --
// defined once here so that logic can't drift between the two.
func (p *RegistryProxy) newReverseProxy(stripPrefix string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(p.target)
	baseDirector := proxy.Director
	targetHost := p.target.Host

	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		if stripPrefix != "" {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, stripPrefix)
		}
		// net/http uses req.Host (falling back to req.URL.Host) when writing
		// the outgoing request; the base Director never touches req.Host, so
		// without this the Registry would see the Engine's inbound Host
		// instead of its own -- breaking any Host-based routing on that side.
		req.Host = targetHost
		// X-Forwarded-For is intentionally NOT set here: httputil.ReverseProxy
		// already appends the client IP to it in ServeHTTP itself (has done so
		// since early Go versions), so adding it in the Director would double
		// up the header.
	}
	return proxy
}

// stripCORSHeaders removes CORS headers coming from the Registry, since the
// Engine applies its own CORS middleware to all proxy routes. Shared by
// Forward and ForwardAndInspect so the header list can't drift between them.
func stripCORSHeaders(res *http.Response) {
	res.Header.Del("Access-Control-Allow-Origin")
	res.Header.Del("Access-Control-Allow-Methods")
	res.Header.Del("Access-Control-Allow-Headers")
	res.Header.Del("Access-Control-Expose-Headers")
	res.Header.Del("Access-Control-Allow-Credentials")
	res.Header.Del("Access-Control-Max-Age")
}

// Forwarder is the subset of RegistryProxy's behavior the proxy handlers
// depend on. Handlers accept this interface rather than *RegistryProxy so
// their tests can substitute a mock instead of standing up a real Registry.
type Forwarder interface {
	Forward(w http.ResponseWriter, r *http.Request, stripPrefix string)
	// ForwardAndInspect is Forward's response-inspecting sibling -- see its
	// doc comment on *RegistryProxy for what onSuccess receives and when.
	ForwardAndInspect(w http.ResponseWriter, r *http.Request, stripPrefix string, onSuccess func(body []byte))
}

// statusRecorder wraps an http.ResponseWriter to capture the status code a
// proxied response was written with. http.ResponseWriter alone doesn't expose
// what was already written, but handlers need the outcome after Forward
// returns to attach it to an OTEL span -- this is the one place that
// capturing happens, shared by both proxy handlers.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	// Default to 200: net/http treats an omitted WriteHeader call as an
	// implicit 200 once the handler writes a body, so this mirrors that.
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// outcomeLabel maps an HTTP status to the coarse "success"/"error" label used
// in OTEL span attributes -- callers care whether the mutation succeeded, not
// the exact status code (which is also captured, separately, if needed).
func outcomeLabel(status int) string {
	if status >= 200 && status < 400 {
		return "success"
	}
	return "error"
}
