package cache

import (
	"strings"
	"sync"
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
	value     any
	expiresAt time.Time
	version   uint64
}

// InMemoryCache provides a simple thread-safe, memory-based cache.
type InMemoryCache struct {
	mu    sync.RWMutex
	items map[string]item
	now   func() time.Time
	next  uint64
}

// NewInMemoryCache creates a new InMemoryCache.
func NewInMemoryCache() *InMemoryCache {
	return &InMemoryCache{
		items: make(map[string]item),
		now:   time.Now,
	}
}

// Set adds an item to the cache with the given TTL.
// A TTL of 0 means the item never expires.
func (c *InMemoryCache) Set(key string, val any, ttl time.Duration) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.items[key] = item{
		value:     val,
		expiresAt: expiresAt,
		version:   c.next,
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

	if !itm.expiresAt.IsZero() && !c.now().Before(itm.expiresAt) {
		// Expiration is fixed at insertion so a missed invalidation cannot let a
		// frequently-read stale entry live forever.
		c.deleteVersion(key, itm.version)
		return nil, false
	}

	return itm.value, true
}

// deleteVersion removes only the expired generation observed by Get; a
// concurrent Set must not be erased by a reader holding an older item copy.
func (c *InMemoryCache) deleteVersion(key string, version uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.items[key]; ok && current.version == version {
		delete(c.items, key)
	}
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
