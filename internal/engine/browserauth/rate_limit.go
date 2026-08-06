package browserauth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type RequestLimiter struct {
	mu           sync.Mutex
	window       time.Duration
	windowStart  time.Time
	maxPerClient int
	maxGlobal    int
	global       int
	clients      map[string]int
	now          func() time.Time
}

func NewRequestLimiter(maxPerClient, maxGlobal int, window time.Duration) *RequestLimiter {
	return &RequestLimiter{
		window: window, maxPerClient: maxPerClient, maxGlobal: maxGlobal,
		clients: make(map[string]int), now: time.Now,
	}
}

func (l *RequestLimiter) Allow(r *http.Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= l.window {
		l.windowStart, l.global, l.clients = now, 0, make(map[string]int)
	}
	client := requestClient(r)
	if l.maxGlobal <= l.global || l.maxPerClient <= l.clients[client] {
		return false
	}
	l.global++
	l.clients[client]++
	return true
}

func requestClient(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
