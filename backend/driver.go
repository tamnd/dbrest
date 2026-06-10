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
