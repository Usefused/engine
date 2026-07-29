package cache

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CacheService defines the interface for our caching layer.
type CacheService interface {
	Set(key string, val any, ttl time.Duration)
	Get(key string) (any, bool)
	Delete(key string)
	DeletePrefix(prefix string)
	Clear()
}

type item struct {
	value      any
	ttl        time.Duration
	expiration *atomic.Int64 // unix nano
}

// InMemoryCache provides a simple thread-safe, memory-based cache.
type InMemoryCache struct {
	mu    sync.RWMutex
	items map[string]item
}

// NewInMemoryCache creates a new InMemoryCache.
func NewInMemoryCache() *InMemoryCache {
	return &InMemoryCache{
		items: make(map[string]item),
	}
}

// Set adds an item to the cache with the given TTL.
// A TTL of 0 means the item never expires.
func (c *InMemoryCache) Set(key string, val any, ttl time.Duration) {
	exp := &atomic.Int64{}
	if ttl > 0 {
		exp.Store(time.Now().Add(ttl).UnixNano())
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = item{
		value:      val,
		ttl:        ttl,
		expiration: exp,
	}
}

// Get retrieves an item from the cache. Returns false if not found or expired.
func (c *InMemoryCache) Get(key string) (any, bool) {
	c.mu.RLock()
	itm, found := c.items[key]
	c.mu.RUnlock()

	if !found {
		return nil, false
	}

	// Check expiration
	exp := itm.expiration.Load()
	if exp > 0 && time.Now().UnixNano() > exp {
		// Lazily delete expired item
		c.Delete(key)
		return nil, false
	}

	// Sliding expiration
	if itm.ttl > 0 {
		itm.expiration.Store(time.Now().Add(itm.ttl).UnixNano())
	}

	return itm.value, true
}

// Delete removes an item from the cache.
func (c *InMemoryCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// DeletePrefix removes all items whose keys start with the given prefix.
func (c *InMemoryCache) DeletePrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
		}
	}
}

// Clear removes all items from the cache.
func (c *InMemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]item)
}
