package backend

import (
	"fmt"
	"sort"
	"sync"
)

// Driver is the factory interface each backend package registers.
// It mirrors the database/sql driver pattern: import the driver package for
// its side-effect (init registers the driver), then open it by name.
type Driver interface {
	Open(dsn string) (Backend, error)
}

// OpenOptions carries cross-cutting open-time settings a driver may honor. A
// field left at its nil/zero value means "use the driver default", so a caller
// can pass only the settings it cares about.
type OpenOptions struct {
	// PreparedStatements, when non-nil, enables or disables server-side prepared
	// statements (PostgREST's db-prepared-statements). A driver that cannot vary
	// this ignores it.
	PreparedStatements *bool
}

// OptionsDriver is an optional extension a Driver implements to receive
// OpenOptions. A driver that does not implement it is opened through Open and the
// options are dropped, so OpenWith stays safe for every backend.
type OptionsDriver interface {
	OpenWithOptions(dsn string, opts OpenOptions) (Backend, error)
}

var (
	driversMu sync.RWMutex
	drivers   = make(map[string]Driver)
)

// Register makes a backend driver available under the given name.
// It panics if name is empty or the same name is registered twice, matching
// the database/sql convention so mis-wired init calls fail loudly at startup.
func Register(name string, d Driver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if name == "" {
		panic("backend: Register called with empty name")
	}
	if _, dup := drivers[name]; dup {
		panic("backend: Register called twice for driver " + name)
	}
	drivers[name] = d
}

// Open opens a Backend using the named driver and the given DSN.
// The driver must have been registered (typically by importing its package).
func Open(name, dsn string) (Backend, error) {
	driversMu.RLock()
	d, ok := drivers[name]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("backend: unknown driver %q (forgotten import?)", name)
	}
	return d.Open(dsn)
}

// OpenWith opens a Backend using the named driver, the given DSN, and the
// supplied open-time options. A driver that implements OptionsDriver receives the
// options; one that does not is opened through Open, ignoring them. This keeps the
// caller engine-agnostic: it always passes the options, and each backend honors
// what it can.
func OpenWith(name, dsn string, opts OpenOptions) (Backend, error) {
	driversMu.RLock()
	d, ok := drivers[name]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("backend: unknown driver %q (forgotten import?)", name)
	}
	if od, ok := d.(OptionsDriver); ok {
		return od.OpenWithOptions(dsn, opts)
	}
	return d.Open(dsn)
}

// Drivers returns a sorted list of registered driver names.
func Drivers() []string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	list := make([]string, 0, len(drivers))
	for name := range drivers {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}
