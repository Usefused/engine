package browserauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestLimiterBoundsClientAndGlobalTraffic(t *testing.T) {
	limiter := NewRequestLimiter(2, 3, time.Minute)
	request := httptest.NewRequest(http.MethodPost, "/auth/managed/start", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	if !limiter.Allow(request) || !limiter.Allow(request) || limiter.Allow(request) {
		t.Fatal("per-client rate limit was not enforced")
	}
	other := httptest.NewRequest(http.MethodPost, "/auth/managed/start", nil)
	other.RemoteAddr = "192.0.2.11:1234"
	if !limiter.Allow(other) || limiter.Allow(other) {
		t.Fatal("global rate limit was not enforced")
	}
}
