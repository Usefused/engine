package api

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

// proxyResponseCache is a short-TTL in-memory cache for non-mutation GraphQL
// responses. It absorbs repeated identical reads (e.g. concurrent workspace
// plans for the same config, or the same dashboard polling quickly) without
// hitting the Registry at all. TTL is intentionally short (5 s) to avoid
// serving stale results while still collapsing burst traffic.
//
// The cache is created once per GraphQLProxyHandler invocation (i.e. once per
// server startup) and lives for the lifetime of the process.
type proxyResponseCache struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List
	ttl      time.Duration
	capacity int
}

type proxyCacheEntry struct {
	key       string
	body      []byte
	status    int
	expiresAt time.Time
}

func newProxyResponseCache(ttl time.Duration) *proxyResponseCache {
	return &proxyResponseCache{
		entries:  make(map[string]*list.Element, 1024),
		order:    list.New(),
		ttl:      ttl,
		capacity: 1024,
	}
}

func (c *proxyResponseCache) get(key string) (proxyCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return proxyCacheEntry{}, false
	}
	entry := element.Value.(proxyCacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.remove(element)
		return proxyCacheEntry{}, false
	}
	c.order.MoveToFront(element)
	return entry, true
}

func (c *proxyResponseCache) set(key string, status int, body []byte) {
	e := proxyCacheEntry{key: key, body: append([]byte(nil), body...), status: status, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		element.Value = e
		c.order.MoveToFront(element)
		return
	}
	c.entries[key] = c.order.PushFront(e)
	if c.order.Len() > c.capacity {
		c.remove(c.order.Back())
	}
}

func (c *proxyResponseCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(proxyCacheEntry)
	delete(c.entries, entry.key)
	c.order.Remove(element)
}

// proxyCacheKey partitions Registry responses by local subject and access
// revision so differently scoped users in one workspace never share results.
func proxyCacheKey(body []byte, actor accesscontrol.Actor) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte(actor.AccountID.String()))
	h.Write([]byte(actor.SubjectID.String()))
	h.Write([]byte(strconv.FormatInt(actor.Authorization.Revision, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// bufferedResponseWriter delays flushing Registry read responses until the
// Engine can attach Server-Timing. Reads are already cacheable and small enough
// to buffer here; mutations still stream through the existing path.
type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: http.Header{}, status: http.StatusOK}
}

func (bw *bufferedResponseWriter) Header() http.Header {
	return bw.header
}

func (bw *bufferedResponseWriter) WriteHeader(status int) {
	bw.status = status
}

func (bw *bufferedResponseWriter) Write(b []byte) (int, error) {
	return bw.body.Write(b)
}

func (bw *bufferedResponseWriter) flushTo(w http.ResponseWriter) {
	copyHeaders(w.Header(), bw.header)
	w.WriteHeader(bw.status)
	_, _ = w.Write(bw.body.Bytes())
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// GraphQLProxyHandler validates the caller's API key against the Engine's own
// store, then forwards POST /graphql through the licence-identity proxy.
//
// Why validate here instead of letting the Registry reject bad keys: the
// Registry is an internal service and should not be a public auth boundary
// for the Engine's users. Rejecting invalid keys here means unauthenticated
// traffic never reaches the Registry at all.
func GraphQLProxyHandler(proxy Forwarder, s store.Store) http.HandlerFunc {
	// Short-TTL response cache for non-mutation reads. Created once per server
	// startup; TTL of 5 s is enough to collapse burst traffic (repeated workspace
	// plans, polling dashboards) without risking stale results for write-after-read.
	cache := newProxyResponseCache(5 * time.Second)

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		accountID, err := controlActorAccount(r.Context())
		authDur := time.Since(start)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.WarnContext(r.Context(), "GraphQLProxyHandler: rejected request with invalid API key", slog.Any("error", err))
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}

		body, err := readAndRestoreBody(r)
		if err != nil {
			http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
			return
		}

		operation, err := authorizeRegistryGraphQLOperation(r.Context(), body)
		if err != nil {
			if !errors.Is(err, errInvalidRegistryGraphQLRequest) {
				accesscontrol.WriteAuthorizationError(w, err)
				return
			}
			http.Error(w, `{"error":"invalid GraphQL operation"}`, http.StatusBadRequest)
			return
		}
		if operation != "mutation" {
			forwardGraphQLRead(proxy, cache, w, r, body, authDur, start)
			return
		}

		forwardMutationWithSpan(proxy, w, r, accountID)
	}
}

func forwardGraphQLRead(proxy Forwarder, cache *proxyResponseCache, w http.ResponseWriter, r *http.Request, body []byte, authDur time.Duration, start time.Time) {
	cacheStart := time.Now()
	cacheKey, cacheable := graphQLRequestCacheKey(r.Context(), body)
	if entry, ok := cache.get(cacheKey); cacheable && ok {
		timing := graphQLProxyTiming{auth: authDur, cache: time.Since(cacheStart), total: time.Since(start), hit: true}
		writeGraphQLCacheHit(w, entry, timing)
		return
	}

	registryStart := time.Now()
	bw := newBufferedResponseWriter()
	proxy.Forward(bw, r, "")
	timing := graphQLProxyTiming{auth: authDur, cache: time.Since(cacheStart), registry: time.Since(registryStart), total: time.Since(start)}
	setGraphQLServerTiming(bw.Header(), timing)
	bw.flushTo(w)
	if cacheable && bw.status == http.StatusOK {
		cache.set(cacheKey, bw.status, bw.body.Bytes())
	}
}

func graphQLRequestCacheKey(ctx context.Context, body []byte) (string, bool) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return "", false
	}
	return proxyCacheKey(body, actor), true
}

func writeGraphQLCacheHit(w http.ResponseWriter, entry proxyCacheEntry, timing graphQLProxyTiming) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "HIT")
	setGraphQLServerTiming(w.Header(), timing)
	w.WriteHeader(entry.status)
	w.Write(entry.body) //nolint:errcheck
}

type graphQLProxyTiming struct {
	auth     time.Duration
	cache    time.Duration
	registry time.Duration
	total    time.Duration
	hit      bool
}

func setGraphQLServerTiming(header http.Header, timing graphQLProxyTiming) {
	parts := []string{
		serverTimingMetric("engine_auth", timing.auth),
		serverTimingMetric("engine_cache", timing.cache),
	}
	if timing.hit {
		parts = append(parts, `engine_cache_hit;desc="hit"`)
	} else {
		parts = append(parts, serverTimingMetric("registry", timing.registry))
	}
	parts = append(parts, serverTimingMetric("engine_total", timing.total))
	header.Set("Server-Timing", strings.Join(parts, ", "))
}

func serverTimingMetric(name string, duration time.Duration) string {
	return name + ";dur=" + strconv.FormatFloat(float64(duration.Microseconds())/1000, 'f', 2, 64)
}

// forwardMutationWithSpan wraps a mutation forward in an OTEL span carrying
// the audit attributes required for user/agent-triggered writes: who did it
// (account_id), what path they hit, and whether it succeeded. Split out from
// GraphQLProxyHandler to keep that function's branching simple.
func forwardMutationWithSpan(proxy Forwarder, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.proxy.graphql_mutation", trace.WithAttributes(
		attribute.String("user_action", "graphql.mutation"),
		attribute.String("account_id", accountID.String()),
		attribute.String("path", r.URL.Path),
	))
	defer span.End()

	rec := newStatusRecorder(w)
	proxy.Forward(rec, r.WithContext(ctx), "")
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}

// readAndRestoreBody reads r.Body fully and replaces it with a fresh reader.
// Detecting a mutation requires reading the body, but Forward still needs an
// intact, unread body to relay to the Registry -- so the bytes read here are
// put back before returning.
func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
