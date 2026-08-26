package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

const graphQLFreshnessTestBody = `{"query":"query RecoveryRead { serviceWebhookEditor(service_id:\"private-service\",version:\"private-version\") { revision } }"}`

// TestGraphQLCacheBypassDirectives distinguishes real directives from quoted extension values and substrings.
func TestGraphQLCacheBypassDirectives(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "absent"},
		{name: "no cache", values: []string{"no-cache"}, want: true},
		{name: "mixed case", values: []string{"max-age=0, No-CaChe"}, want: true},
		{name: "repeated values", values: []string{"max-age=60", "private, NO-STORE"}, want: true},
		{name: "whitespace", values: []string{"\t no-store \t , max-age=5"}, want: true},
		{name: "directive value", values: []string{`no-cache="field"`}, want: true},
		{name: "unrelated tokens", values: []string{"x-no-cache, no-store-extra, max-age=0"}},
		{name: "quoted directive names", values: []string{`extension="x,no-cache,no-store,y", max-age=5`}},
		{name: "escaped quoted value", values: []string{`extension="x\",no-cache,y", max-age=5`}},
		{name: "directive after quoted value", values: []string{`extension="x,no-store,y", no-cache`}, want: true},
	}
	for _, test := range tests {
		// Each case uses separate header values to exercise net/http's repeated-header representation.
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{"Cache-Control": test.values}
			// Only exact unquoted directive names can change the proxy's cache policy.
			if got := graphQLCacheBypassRequested(header); got != test.want {
				t.Fatalf("cache bypass = %t, want %t", got, test.want)
			}
		})
	}
}

// TestGraphQLProxyFreshnessBypassesCachedReads proves repeated recovery reads reach Registry despite a cached baseline.
func TestGraphQLProxyFreshnessBypassesCachedReads(t *testing.T) {
	for _, directive := range []string{"no-cache", "no-store"} {
		// Both freshness directives share identical lookup and insertion behavior.
		t.Run(directive, func(t *testing.T) {
			actor := graphQLProxyTestActor(t, uuid.New(), uuid.New(), 1)
			forwarder := &mockForwarder{}
			handler := GraphQLProxyHandler(forwarder, &mockKeyStore{})
			serveGraphQLFreshnessRead(handler, actor)
			for range 2 {
				response := serveGraphQLFreshnessRead(handler, actor, directive)
				// A bypass must never report the pre-existing cached response as a hit.
				if response.Code != http.StatusOK || response.Header().Get("X-Cache") == "HIT" {
					t.Fatalf("fresh read = %d/%q", response.Code, response.Header().Get("X-Cache"))
				}
			}
			cached := serveGraphQLFreshnessRead(handler, actor)
			// Ordinary reads keep the original burst-collapse behavior after recovery requests.
			if forwarder.calls != 3 || cached.Header().Get("X-Cache") != "HIT" {
				t.Fatalf("forwards/cache = %d/%q, want 3/HIT", forwarder.calls, cached.Header().Get("X-Cache"))
			}
		})
	}
}

// TestGraphQLProxyFreshnessDoesNotPopulateCache proves bypass reads cannot create a later normal cache hit.
func TestGraphQLProxyFreshnessDoesNotPopulateCache(t *testing.T) {
	for _, directive := range []string{"no-cache", "no-store"} {
		// An empty cache isolates insertion behavior from the independently tested lookup bypass.
		t.Run(directive, func(t *testing.T) {
			actor := graphQLProxyTestActor(t, uuid.New(), uuid.New(), 1)
			forwarder := &mockForwarder{}
			handler := GraphQLProxyHandler(forwarder, &mockKeyStore{})
			serveGraphQLFreshnessRead(handler, actor, directive)
			serveGraphQLFreshnessRead(handler, actor, directive)
			firstNormal := serveGraphQLFreshnessRead(handler, actor)
			// Neither bypass response may populate the cache, so the first normal read still forwards.
			if forwarder.calls != 3 || firstNormal.Header().Get("X-Cache") == "HIT" {
				t.Fatalf("first normal read forwards/cache = %d/%q", forwarder.calls, firstNormal.Header().Get("X-Cache"))
			}
			secondNormal := serveGraphQLFreshnessRead(handler, actor)
			// A normal successful response remains cacheable without any global invalidation redesign.
			if forwarder.calls != 3 || secondNormal.Header().Get("X-Cache") != "HIT" {
				t.Fatalf("second normal read forwards/cache = %d/%q", forwarder.calls, secondNormal.Header().Get("X-Cache"))
			}
		})
	}
}

// TestGraphQLProxyFreshnessAuthorizationPrecedesBypass keeps unauthorized recovery requests away from Registry and telemetry.
func TestGraphQLProxyFreshnessAuthorizationPrecedesBypass(t *testing.T) {
	exporter := setupTestTracer(t)
	actor := graphQLProxyTestActor(t, uuid.New(), uuid.New(), 1)
	forwarder := &mockForwarder{}
	handler := GraphQLProxyHandler(forwarder, &mockKeyStore{})
	serveGraphQLFreshnessRead(handler, actor)
	actor.Authorization = accesscontrol.AuthorizationSnapshot{Revision: actor.Authorization.Revision}
	ctx, span := otel.Tracer("test").Start(context.Background(), "denied request")
	request := graphQLFreshnessRequest(actor, "no-cache")
	request = request.WithContext(accesscontrol.ContextWithActor(ctx, actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	span.End()
	// A denied actor cannot reuse a same-partition cached response or forward by requesting freshness.
	if response.Code != http.StatusForbidden || forwarder.calls != 1 {
		t.Fatalf("denied recovery status/forwards = %d/%d", response.Code, forwarder.calls)
	}
	spans := exporter.GetSpans()
	// Rejected reads retain their existing authorization event without adding a cache-bypass event.
	if len(spans) != 1 || len(spans[0].Events) != 1 || spans[0].Events[0].Name != "engine.authorization.check" {
		t.Fatalf("denied recovery trace = %#v", spans)
	}
}

// TestGraphQLProxyFreshnessTelemetryStaysOnCallerSpan records only fixed cache policy facts.
func TestGraphQLProxyFreshnessTelemetryStaysOnCallerSpan(t *testing.T) {
	exporter := setupTestTracer(t)
	actor := graphQLProxyTestActor(t, uuid.New(), uuid.New(), 1)
	handler := GraphQLProxyHandler(&mockForwarder{}, &mockKeyStore{})
	ctx, span := otel.Tracer("test").Start(context.Background(), "recovery request")
	request := graphQLFreshnessRequest(actor, "private, no-cache, private-extension=secret-header-value")
	request = request.WithContext(accesscontrol.ContextWithActor(ctx, actor))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	span.End()
	spans := exporter.GetSpans()
	// Cache recovery follows the existing authorization event without creating another request span.
	if len(spans) != 1 || len(spans[0].Events) != 2 || spans[0].Events[0].Name != "engine.authorization.check" {
		t.Fatalf("freshness trace = %#v", spans)
	}
	event := spans[0].Events[1]
	// Fixed event identity and exact cardinality exclude request headers, GraphQL source, and cache keys.
	if event.Name != "engine.proxy.graphql_cache.bypass" || len(event.Attributes) != 2 {
		t.Fatalf("freshness event = %#v", event)
	}
	want := map[string]string{"cache.kind": "registry_graphql_response", "outcome": "bypassed"}
	for _, field := range event.Attributes {
		// Exact values prevent caller-controlled text from entering even an allowlisted attribute name.
		if expected, ok := want[string(field.Key)]; !ok || field.Value.AsString() != expected {
			t.Fatalf("unexpected freshness attribute: %#v", field)
		}
		delete(want, string(field.Key))
	}
	// Removing verified fields also detects duplicate attributes replacing a required fixed dimension.
	if len(want) != 0 {
		t.Fatalf("missing freshness attributes: %#v", want)
	}
}

// graphQLFreshnessDelayedForwarder reuses the ordinary mock contract while controlling only response completion order.
type graphQLFreshnessDelayedForwarder struct {
	mockForwarder
	started     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

// Forward delays only a bypass response so a newer normal read can populate the cache first.
func (f *graphQLFreshnessDelayedForwarder) Forward(w http.ResponseWriter, r *http.Request, _ string) {
	// A suspended recovery read models a slow Registry response without sleeps or nondeterministic timing.
	if r.Header.Get("Cache-Control") == "no-cache" {
		close(f.started)
		<-f.release
		_, _ = w.Write([]byte(`{"data":{"revision":"delayed"}}`))
		return
	}
	_, _ = w.Write([]byte(`{"data":{"revision":"current"}}`))
}

// unblock guarantees a failed test still releases its pending request exactly once.
func (f *graphQLFreshnessDelayedForwarder) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// TestGraphQLProxyDelayedFreshnessResponseCannotOverwriteCache protects the write fence after upstream completion.
func TestGraphQLProxyDelayedFreshnessResponseCannotOverwriteCache(t *testing.T) {
	actor := graphQLProxyTestActor(t, uuid.New(), uuid.New(), 1)
	forwarder := &graphQLFreshnessDelayedForwarder{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(forwarder.unblock)
	handler := GraphQLProxyHandler(forwarder, &mockKeyStore{})
	done := make(chan struct{})
	// Distinct recorders isolate concurrent responses while the handler shares its production cache.
	go func() {
		serveGraphQLFreshnessRead(handler, actor, "no-cache")
		close(done)
	}()
	waitForGraphQLFreshnessSignal(t, forwarder.started)
	serveGraphQLFreshnessRead(handler, actor)
	forwarder.unblock()
	waitForGraphQLFreshnessSignal(t, done)
	response := serveGraphQLFreshnessRead(handler, actor)
	// Completion of the older bypass response must not replace the newer cacheable baseline.
	if response.Header().Get("X-Cache") != "HIT" || response.Body.String() != `{"data":{"revision":"current"}}` {
		t.Fatalf("post-recovery cache = %q/%s", response.Header().Get("X-Cache"), response.Body.String())
	}
}

// waitForGraphQLFreshnessSignal bounds channel coordination so regressions fail instead of hanging the suite.
func waitForGraphQLFreshnessSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	// Successful synchronization is deterministic; the timeout exists only as a failed-test escape hatch.
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("GraphQL freshness request did not reach the expected boundary")
	}
}

// graphQLFreshnessRequest preserves one actor and document so tests exercise identical cache partitions.
func graphQLFreshnessRequest(actor accesscontrol.Actor, values ...string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(graphQLFreshnessTestBody))
	for _, value := range values {
		request.Header.Add("Cache-Control", value)
	}
	return request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
}

// serveGraphQLFreshnessRead drives the real authorization and proxy boundary for one isolated request.
func serveGraphQLFreshnessRead(handler http.HandlerFunc, actor accesscontrol.Actor, values ...string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphQLFreshnessRequest(actor, values...))
	return response
}
