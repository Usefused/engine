package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	enginemiddleware "github.com/Usefused/engine/internal/engine/middleware"
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
	mu      sync.RWMutex
	entries map[string]proxyCacheEntry
	ttl     time.Duration
}

type proxyCacheEntry struct {
	body      []byte
	status    int
	expiresAt time.Time
}

func newProxyResponseCache(ttl time.Duration) *proxyResponseCache {
	return &proxyResponseCache{
		entries: make(map[string]proxyCacheEntry),
		ttl:     ttl,
	}
}

func (c *proxyResponseCache) get(key string) (proxyCacheEntry, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return proxyCacheEntry{}, false
	}
	return e, true
}

func (c *proxyResponseCache) set(key string, status int, body []byte) {
	e := proxyCacheEntry{body: body, status: status, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Lock()
	c.entries[key] = e
	c.mu.Unlock()
}

// proxyCacheKey hashes the raw request body together with the accountID so
// that different accounts' responses never alias each other, and two queries
// with different body bytes always get separate cache entries.
func proxyCacheKey(body []byte, accountID uuid.UUID) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte(accountID.String()))
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

// GraphQLProxyHandler validates the caller's API key against the Engine's
// own store, then forwards POST /graphql to the Registry unchanged.
//
// Why validate here instead of letting the Registry reject bad keys: the
// Registry is an internal service and should not be a public auth boundary
// for the Engine's users. Rejecting invalid keys here means unauthenticated
// traffic never reaches the Registry at all.
func GraphQLProxyHandler(proxy Forwarder, s store.Store, enforcers ...*enginemiddleware.RuntimeEnforcer) http.HandlerFunc {
	enforcer := firstRuntimeEnforcer(enforcers)
	// Short-TTL response cache for non-mutation reads. Created once per server
	// startup; TTL of 5 s is enough to collapse burst traffic (repeated workspace
	// plans, polling dashboards) without risking stale results for write-after-read.
	cache := newProxyResponseCache(5 * time.Second)

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		apiKey := r.Header.Get("X-API-Key")
		accountID, err := validateAPIKey(r.Context(), s, apiKey)
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

		if !isMutation(body) {
			forwardGraphQLRead(enforcer, proxy, cache, w, r, accountID, body, authDur, start)
			return
		}

		forwardMutationWithSpan(proxy, enforcer, w, r, accountID)
	}
}

func forwardGraphQLRead(enforcer *enginemiddleware.RuntimeEnforcer, proxy Forwarder, cache *proxyResponseCache, w http.ResponseWriter, r *http.Request, accountID uuid.UUID, body []byte, authDur time.Duration, start time.Time) {
	cacheStart := time.Now()
	cacheKey := proxyCacheKey(body, accountID)
	if entry, ok := cache.get(cacheKey); ok {
		timing := graphQLProxyTiming{auth: authDur, cache: time.Since(cacheStart), total: time.Since(start), hit: true}
		writeGraphQLCacheHit(w, entry, timing)
		return
	}

	registryStart := time.Now()
	bw := newBufferedResponseWriter()
	forwardWithRuntime(enforcer, proxy, bw, r, accountID)
	timing := graphQLProxyTiming{auth: authDur, cache: time.Since(cacheStart), registry: time.Since(registryStart), total: time.Since(start)}
	setGraphQLServerTiming(bw.Header(), timing)
	bw.flushTo(w)
	if bw.status == http.StatusOK {
		cache.set(cacheKey, bw.status, bw.body.Bytes())
	}
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
func forwardMutationWithSpan(proxy Forwarder, enforcer *enginemiddleware.RuntimeEnforcer, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.proxy.graphql_mutation", trace.WithAttributes(
		attribute.String("user_action", "graphql.mutation"),
		attribute.String("account_id", accountID.String()),
		attribute.String("path", r.URL.Path),
	))
	defer span.End()

	rec := newStatusRecorder(w)
	forwardWithRuntime(enforcer, proxy, rec, r.WithContext(ctx), accountID)
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

// isMutation reports whether a GraphQL request body's query is a mutation.
// GraphQL's shorthand query syntax (`{ field }`) is only valid for queries,
// so any operation that isn't explicitly typed with a leading keyword is a
// read -- checking for an explicit "mutation" keyword is sufficient here
// without pulling in a full GraphQL parser just to classify traffic.
func isMutation(body []byte) bool {
	var payload struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	trimmed := strings.TrimSpace(payload.Query)
	return strings.HasPrefix(strings.ToLower(trimmed), "mutation")
}
