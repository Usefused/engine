package middleware

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

func TestCheckRateLimit_ThrottlesPerService(t *testing.T) {
	globalServiceLimiter = &serviceRateLimiter{buckets: map[string]*serviceBucket{}}
	cfg := &fusedobject.RateLimitConfig{Strategy: "token_bucket", RequestsPerSecond: 1}

	if !CheckRateLimit("svc-a", cfg) {
		t.Fatal("first request should be allowed")
	}
	if CheckRateLimit("svc-a", cfg) {
		t.Fatal("second immediate request should be throttled")
	}
	if !CheckRateLimit("svc-b", cfg) {
		t.Fatal("different service should use a separate bucket")
	}
}
