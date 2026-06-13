package schema

import "sync/atomic"

// Cache publishes the schema model. Readers take an immutable snapshot with
// Load and keep using it for the whole request even if a reload lands midway;
// Store swaps in a freshly introspected model atomically, the reload
// mechanism PostgREST drives from SIGUSR1 and NOTIFY on the db-channel. The
// models themselves are never mutated after publication.
type Cache struct {
	p atomic.Pointer[Model]
}

// NewCache publishes the initial model.
func NewCache(m *Model) *Cache {
	c := &Cache{}
	c.p.Store(m)
	return c
}

// Load returns the current model snapshot.
func (c *Cache) Load() *Model { return c.p.Load() }

// Store publishes a new model. In-flight readers keep their snapshot; the
// next Load sees the new one.
func (c *Cache) Store(m *Model) { c.p.Store(m) }
