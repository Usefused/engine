package sandbox

import (
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Minimal token-bucket rate limiter (no external dependency required).
// Uses a sliding-window counter per key, evicted after 2× the window duration.
// ---------------------------------------------------------------------------

type rateLimiterEntry struct {
	mu        sync.Mutex
	tokens    float64
	maxTokens float64
	refillPS  float64 // tokens added per second
	lastCheck time.Time
}

// allow returns true if a token is available, consuming it atomically.
func (e *rateLimiterEntry) allow() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(e.lastCheck).Seconds()
	e.lastCheck = now

	// Refill tokens based on elapsed time.
	e.tokens += elapsed * e.refillPS
	if e.tokens > e.maxTokens {
		e.tokens = e.maxTokens
	}

	if e.tokens < 1 {
		return false
	}
	e.tokens--
	return true
}

type rateLimitStore struct {
	mu      sync.Mutex
	entries map[string]*rateLimiterEntry
	// Configuration — set once at startup.
	maxTokens float64
	refillPS  float64
}

func newRateLimitStore(perMinute, burst int) *rateLimitStore {
	s := &rateLimitStore{
		entries:   make(map[string]*rateLimiterEntry),
		maxTokens: float64(burst),
		refillPS:  float64(perMinute) / 60.0,
	}
	// Background eviction: remove idle entries every 5 minutes.
	go s.evictLoop()
	return s
}

func (s *rateLimitStore) allow(key string) bool {
	s.mu.Lock()
	e, ok := s.entries[key]
	if !ok {
		e = &rateLimiterEntry{
			tokens:    s.maxTokens, // new key starts with a full burst
			maxTokens: s.maxTokens,
			refillPS:  s.refillPS,
			lastCheck: time.Now(),
		}
		s.entries[key] = e
	}
	s.mu.Unlock()
	return e.allow()
}

func (s *rateLimitStore) evictLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-10 * time.Minute)
		s.mu.Lock()
		for key, e := range s.entries {
			e.mu.Lock()
			idle := e.lastCheck.Before(cutoff)
			e.mu.Unlock()
			if idle {
				delete(s.entries, key)
			}
		}
		s.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Global rate limiter instances — initialised by initRateLimiters().
// ---------------------------------------------------------------------------

var (
	// sseRateLimiter limits new SSE connections per SDK-ID.
	sseRateLimiter *rateLimitStore
	// messageRateLimiter limits tools/call messages per SDK-ID.
	messageRateLimiter *rateLimitStore
)

// initRateLimiters creates the global rate limiter stores from config values.
func initRateLimiters(ssePerMinute, sseBurst, msgPerMinute, msgBurst int) {
	sseRateLimiter = newRateLimitStore(ssePerMinute, sseBurst)
	messageRateLimiter = newRateLimitStore(msgPerMinute, msgBurst)
}

// ---------------------------------------------------------------------------
// Guard helpers — call these at the top of each handler.
// ---------------------------------------------------------------------------

// allowSSEConnect returns true and does nothing if the SDK-ID is within the
// SSE connection rate limit. Otherwise it writes HTTP 429 and returns false.
func allowSSEConnect(w http.ResponseWriter, appID string) bool {
	if sseRateLimiter != nil && !sseRateLimiter.allow(appID) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many connections for this MCP server, please slow down")
		return false
	}
	return true
}

// allowMessage returns true and does nothing if the SDK-ID is within the
// message-rate limit. Otherwise it writes HTTP 429 and returns false.
func allowMessage(w http.ResponseWriter, appID string) bool {
	if messageRateLimiter != nil && !messageRateLimiter.allow(appID) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "message rate limit exceeded for this MCP server")
		return false
	}
	return true
}
