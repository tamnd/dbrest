package auth

import "sync"

// jwtCache is a bounded, fixed-size cache of verified-token outcomes with SIEVE
// eviction, the v14 performance change (spec 13). It caches only the resolved
// claim set keyed by the raw token, never persists, and is scoped to the
// process. Time validity is re-checked by the verifier on every hit, so an entry
// never extends a token's lifetime; the cache only skips signature work.
//
// SIEVE keeps a queue of entries (newest at head, oldest at tail) and a "hand"
// that sweeps from the tail toward the head. A hit sets the entry's visited bit;
// eviction clears visited bits as the hand passes them and removes the first
// unvisited entry it finds. This approximates LRU at the cost of a single bit
// and a pointer, with no per-hit list surgery.
type jwtCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*cacheEntry
	head     *cacheEntry // newest
	tail     *cacheEntry // oldest
	hand     *cacheEntry // eviction cursor
}

// cacheEntry is one cached verification. next points toward the tail (older),
// prev toward the head (newer).
type cacheEntry struct {
	key     string
	claims  map[string]any
	visited bool
	prev    *cacheEntry
	next    *cacheEntry
}

// newJWTCache builds an empty cache with a fixed capacity.
func newJWTCache(capacity int) *jwtCache {
	return &jwtCache{capacity: capacity, items: make(map[string]*cacheEntry, capacity)}
}

// get returns the cached claims for a token and marks the entry visited so the
// next eviction sweep spares it.
func (c *jwtCache) get(key string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e.visited = true
	return e.claims, true
}

// put inserts a verification outcome, evicting one entry first when the cache is
// full. A re-inserted key refreshes its claims and marks it visited.
func (c *jwtCache) put(key string, claims map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity <= 0 {
		return
	}
	if e, ok := c.items[key]; ok {
		e.claims = claims
		e.visited = true
		return
	}
	if len(c.items) >= c.capacity {
		c.evict()
	}
	e := &cacheEntry{key: key, claims: claims, next: c.head}
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
	c.items[key] = e
}

// evict removes one entry by the SIEVE rule: advance the hand from its position
// (or the tail) toward the head, clearing visited bits, and drop the first
// unvisited entry. The bit-clearing pass guarantees termination.
func (c *jwtCache) evict() {
	obj := c.hand
	if obj == nil {
		obj = c.tail
	}
	for obj != nil && obj.visited {
		obj.visited = false
		obj = obj.prev
		if obj == nil {
			obj = c.tail
		}
	}
	if obj == nil {
		return
	}
	c.hand = obj.prev
	c.remove(obj)
}

// remove unlinks an entry from the queue and the index.
func (c *jwtCache) remove(e *cacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.tail = e.prev
	}
	delete(c.items, e.key)
}

// len reports the number of cached entries.
func (c *jwtCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
