package accesscontrol

import (
	"container/list"
	"sync"
	"time"
)

type boundedCache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	entries  map[K]*list.Element
	order    *list.List
}

type boundedCacheEntry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time
}

func newBoundedCache[K comparable, V any](capacity int, ttl time.Duration, now func() time.Time) *boundedCache[K, V] {
	return &boundedCache[K, V]{
		capacity: capacity,
		ttl:      ttl,
		now:      now,
		entries:  make(map[K]*list.Element, capacity),
		order:    list.New(),
	}
}

func (c *boundedCache[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	entry := element.Value.(boundedCacheEntry[K, V])
	if !entry.expiresAt.After(c.now()) {
		c.remove(element)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(element)
	return entry.value, true
}

func (c *boundedCache[K, V]) set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := boundedCacheEntry[K, V]{key: key, value: value, expiresAt: c.now().Add(c.ttl)}
	if element, ok := c.entries[key]; ok {
		element.Value = entry
		c.order.MoveToFront(element)
		return
	}
	c.entries[key] = c.order.PushFront(entry)
	if c.order.Len() > c.capacity {
		c.remove(c.order.Back())
	}
}

func (c *boundedCache[K, V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[K]*list.Element, c.capacity)
	c.order.Init()
}

func (c *boundedCache[K, V]) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(boundedCacheEntry[K, V])
	delete(c.entries, entry.key)
	c.order.Remove(element)
}
