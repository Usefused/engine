package middleware

import (
	"fmt"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

type serviceBucket struct {
	tokens    float64
	capacity  float64
	refillPS  float64
	lastCheck time.Time
}

type serviceRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*serviceBucket
}

var globalServiceLimiter = &serviceRateLimiter{buckets: map[string]*serviceBucket{}}

// CheckRateLimit enforces the provider-declared service-level limit with one
// Engine-local token bucket per service/config pair. A nil or empty config is
// intentionally unlimited.
func CheckRateLimit(serviceID string, globalConfig *fusedobject.RateLimitConfig) bool {
	if serviceID == "" || globalConfig == nil {
		return true
	}
	refillPS := refillPerSecond(globalConfig)
	if refillPS <= 0 {
		return true
	}
	key := fmt.Sprintf("%s:%d:%d", serviceID, globalConfig.RequestsPerSecond, globalConfig.RequestsPerMinute)
	return globalServiceLimiter.allow(key, refillPS, bucketCapacity(globalConfig, refillPS))
}

func refillPerSecond(config *fusedobject.RateLimitConfig) float64 {
	if config.RequestsPerSecond > 0 {
		return float64(config.RequestsPerSecond)
	}
	if config.RequestsPerMinute > 0 {
		return float64(config.RequestsPerMinute) / 60
	}
	return 0
}

func bucketCapacity(config *fusedobject.RateLimitConfig, refillPS float64) float64 {
	if config.RequestsPerSecond > 0 {
		return float64(config.RequestsPerSecond)
	}
	if config.RequestsPerMinute > 0 {
		return float64(config.RequestsPerMinute)
	}
	return refillPS
}

func (l *serviceRateLimiter) allow(key string, refillPS, capacity float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	bucket := l.buckets[key]
	if bucket == nil {
		bucket = &serviceBucket{tokens: capacity, capacity: capacity, refillPS: refillPS, lastCheck: now}
		l.buckets[key] = bucket
	}

	elapsed := now.Sub(bucket.lastCheck).Seconds()
	bucket.lastCheck = now
	bucket.tokens += elapsed * bucket.refillPS
	if bucket.tokens > bucket.capacity {
		bucket.tokens = bucket.capacity
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}
