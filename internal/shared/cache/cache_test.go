package cache

import (
	"testing"
	"time"
)

func TestInMemoryCacheUsesAbsoluteExpiration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := NewInMemoryCache()
	cache.now = func() time.Time { return now }
	cache.Set("runtime", "value", 30*time.Second)

	now = now.Add(20 * time.Second)
	if _, ok := cache.Get("runtime"); !ok {
		t.Fatal("entry expired before its insertion-relative deadline")
	}
	now = now.Add(10 * time.Second)
	if _, ok := cache.Get("runtime"); ok {
		t.Fatal("a read extended the absolute expiration deadline")
	}
}

func TestInMemoryCacheZeroTTLDoesNotExpire(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := NewInMemoryCache()
	cache.now = func() time.Time { return now }
	cache.Set("static", "value", 0)

	now = now.Add(24 * time.Hour)
	value, ok := cache.Get("static")
	if !ok || value != "value" {
		t.Fatalf("zero-TTL entry = (%v, %v), want (value, true)", value, ok)
	}
}

func TestInMemoryCacheExpiredGenerationCannotDeleteReplacement(t *testing.T) {
	cache := NewInMemoryCache()
	cache.Set("runtime", "old", time.Second)
	oldVersion := cache.items["runtime"].version
	cache.Set("runtime", "new", time.Second)

	// Get releases its read lock before lazy cleanup. Version fencing keeps an
	// expired reader from deleting a Set that won the intervening race.
	cache.deleteVersion("runtime", oldVersion)
	value, ok := cache.Get("runtime")
	if !ok || value != "new" {
		t.Fatalf("replacement after stale delete = (%v, %v), want (new, true)", value, ok)
	}
}
