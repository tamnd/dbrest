// Package rpc models database functions as the engine-agnostic descriptors the
// /rpc/<fn> endpoint calls. A function is realized one of two ways (spec 12):
// native discovery from an engine catalog, or a portable registry declared
// outside the engine. The frontend uses only the Registry interface and never
// asks which source a function came from.
//
// This package is a leaf: it imports nothing from the rest of dbrest, so the IR,
// the planner, the SQL compiler, and the backend SPI can all refer to a
// *Function without a cycle.
package rpc

import "sort"

// Volatility classifies a function's effect, which fixes the methods it allows
// and the transaction mode it runs in (spec 12). A registry entry that omits it
// defaults to Volatile, the safe choice (POST only, read-write).
type Volatility uint8

const (
	Volatile  Volatility = iota // side effects: POST only, read-write
	Stable                      // read-only within a statement: GET or POST
	Immutable                   // pure: GET or POST
)

// ReadOnly reports whether a function of this volatility may run through a read
// method and in a read-only transaction.
func (v Volatility) ReadOnly() bool { return v != Volatile }

// SecurityMode is whether a function runs as its caller (the default, matching
// PostgreSQL) or as its definer. Definer enforcement on engines that cannot
// delegate it lives in the authorization layer (spec 14); this package only
// carries the declared mode.
type SecurityMode uint8

const (
	Invoker SecurityMode = iota
	Definer
)

// ReturnKind decides how a function's result is shaped on the wire.
type ReturnKind uint8

const (
	ReturnScalar ReturnKind = iota // returns <type>      -> a single value
	ReturnSetOf                    // returns setof <type> -> an array of values
	ReturnTable                    // returns table(...)   -> an array of objects
)

// ReturnShape is a function's declared result. Type is the canonical type of a
// scalar or setof-scalar return; Columns names a table return's columns so the
// planner can validate a post-filter select/where/order against them (an empty
// Columns means the result shape is taken from the engine, best-effort).
type ReturnShape struct {
	Kind    ReturnKind
	Type    string
	Columns []Column
}

// Column is one column of a table return.
type Column struct {
	Name string
	Type string
}

// Param is one declared parameter, in signature order.
type Param struct {
	Name     string
	Type     string // canonical type (spec 16)
	Optional bool   // may be omitted; Default is bound in its place
	Default  any    // value bound when an optional param is omitted (nil = NULL)
	Variadic bool   // collects the trailing values (not yet lowered; see notes)
}

// Function is one callable function descriptor. Exactly one realization is set;
// this slice implements the portable Query (native discovery from an engine
// catalog is a later slice).
type Function struct {
	Name       string
	Params     []Param
	Returns    ReturnShape
	Volatility Volatility
	Security   SecurityMode
	Query      *PortableQuery
}

// PortableQuery is a function defined outside the engine: a parameterized SQL
// statement whose :name placeholders bind to the call's arguments. The MongoDB
// pipeline form and the Go-handler form are later slices.
type PortableQuery struct {
	SQL string
}

// Required reports the names of the function's non-optional parameters.
func (f *Function) Required() []string {
	var req []string
	for _, p := range f.Params {
		if !p.Optional {
			req = append(req, p.Name)
		}
	}
	return req
}

// Param returns the named parameter and whether it exists.
func (f *Function) Param(name string) (Param, bool) {
	for _, p := range f.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// ArgSet is the set of argument names present in a request, used to choose an
// overload.
type ArgSet map[string]bool

// Registry resolves an RPC name to a callable function descriptor. The frontend
// uses only this interface, so a native and a portable registry are
// interchangeable behind it.
type Registry interface {
	// Lookup finds the function for a name, choosing an overload by the argument
	// set present in the request. The bool is false when no overload matches; the
	// caller raises PGRST202.
	Lookup(name string, args ArgSet) (*Function, bool)
	// List enumerates the exposed functions in a stable order, for OpenAPI.
	List() []*Function
}

// StaticRegistry is a portable registry built in memory: one or more overloads
// per name, declared programmatically (and, once configuration lands, from
// config). It is the realization for engines with no usable stored-procedure
// discovery, such as SQLite.
type StaticRegistry struct {
	byName map[string][]*Function
	names  []string
}

// NewStaticRegistry builds a registry from a flat list of functions. Two
// functions of the same name are overloads, disambiguated at Lookup by their
// argument sets.
func NewStaticRegistry(fns []*Function) *StaticRegistry {
	r := &StaticRegistry{byName: map[string][]*Function{}}
	for _, f := range fns {
		if _, seen := r.byName[f.Name]; !seen {
			r.names = append(r.names, f.Name)
		}
		r.byName[f.Name] = append(r.byName[f.Name], f)
	}
	sort.Strings(r.names)
	return r
}

// Lookup selects the overload satisfied by the argument set: every required
// parameter present, and no argument naming a parameter the overload does not
// declare. Among satisfiable overloads it prefers an exact parameter-set match,
// then the most specific (largest required set), deterministically.
func (r *StaticRegistry) Lookup(name string, args ArgSet) (*Function, bool) {
	cands := r.byName[name]
	if len(cands) == 0 {
		return nil, false
	}
	var best *Function
	bestScore := -1
	for _, f := range cands {
		if !satisfiable(f, args) {
			continue
		}
		score := len(f.Required())
		if exactMatch(f, args) {
			score += 1000 // an exact parameter-set match wins outright
		}
		if score > bestScore {
			best, bestScore = f, score
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// List returns the functions in stable name order (overloads in declared order).
func (r *StaticRegistry) List() []*Function {
	var out []*Function
	for _, n := range r.names {
		out = append(out, r.byName[n]...)
	}
	return out
}

// satisfiable reports whether an argument set can call this overload: all
// required parameters are present and no argument names an unknown parameter.
func satisfiable(f *Function, args ArgSet) bool {
	for _, req := range f.Required() {
		if !args[req] {
			return false
		}
	}
	for name := range args {
		if _, ok := f.Param(name); !ok {
			return false
		}
	}
	return true
}

// exactMatch reports whether the argument set names exactly the parameters of f.
func exactMatch(f *Function, args ArgSet) bool {
	if len(args) != len(f.Params) {
		return false
	}
	for _, p := range f.Params {
		if !args[p.Name] {
			return false
		}
	}
	return true
}

// EmptyRegistry is a registry with no functions; every Lookup misses. A backend
// that has not been given any functions returns this so the frontend raises a
// clean PGRST202 rather than dereferencing nil.
type EmptyRegistry struct{}

// Lookup always misses: an empty registry has no functions.
func (EmptyRegistry) Lookup(string, ArgSet) (*Function, bool) { return nil, false }

// List returns no functions.
func (EmptyRegistry) List() []*Function { return nil }
