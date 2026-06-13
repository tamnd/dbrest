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

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

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
	ReturnVoid                     // returns void        -> 200 with a null body
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
	Variadic bool   // collects the trailing values into a list, expanded at lowering
	// RawBody marks the single-unnamed-parameter form: PostgREST binds the whole
	// raw request body to this parameter, decoded by Content-Type (a JSON value of
	// any kind for application/json, raw text for text/plain and text/xml, raw
	// bytes for application/octet-stream), rather than treating the body as a JSON
	// object of named arguments. The parameter keeps a name so the SQL body can
	// reference its placeholder.
	RawBody bool
}

// SingleRawBody reports whether the function takes exactly one parameter bound
// from the raw request body, the unnamed-argument form. Such a function receives
// the whole body as that one argument regardless of the body's JSON shape.
func (f *Function) SingleRawBody() (Param, bool) {
	if len(f.Params) == 1 && f.Params[0].RawBody {
		return f.Params[0], true
	}
	return Param{}, false
}

// Function is one callable function descriptor. Exactly one realization is set;
// this slice implements the portable Query (native discovery from an engine
// catalog is a later slice).
type Function struct {
	Name   string
	Params []Param
	// Comment is the database comment on the function (COMMENT ON FUNCTION,
	// or the registry declaration's comment field). The OpenAPI generator
	// splits it into the rpc operation's summary and description, as v14 does.
	Comment    string
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

// Required reports the names of the function's non-optional parameters. A
// variadic parameter is never required: PostgreSQL accepts a variadic call with
// zero trailing arguments, so an omitted variadic still satisfies an overload.
func (f *Function) Required() []string {
	var req []string
	for _, p := range f.Params {
		if !p.Optional && !p.Variadic {
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
	// Resolve is Lookup that also reports ambiguity: it returns the chosen overload
	// (ok true), or ok false with the competing signatures when two overloads are
	// equally good (the caller raises PGRST203), or ok false with no signatures
	// when none match (PGRST202). Lookup is Resolve collapsed to (fn, ok).
	Resolve(name string, args ArgSet) (fn *Function, ambiguous []string, ok bool)
	// List enumerates the exposed functions in a stable order, for OpenAPI.
	List() []*Function
}

// Signature renders the function as PostgREST spells it in a PGRST202/PGRST203
// message: name(param => type, ...), or name() when it takes no parameters. The
// schema, when given, qualifies the name (api.add(...)).
func (f *Function) Signature(schemaName string) string {
	name := f.Name
	if schemaName != "" {
		name = schemaName + "." + name
	}
	parts := make([]string, len(f.Params))
	for i, p := range f.Params {
		parts[i] = p.Name + " => " + p.Type
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
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
	fn, _, ok := r.Resolve(name, args)
	return fn, ok
}

// Resolve selects the overload for an argument set and reports ambiguity. It
// scores every satisfiable overload (an exact parameter-set match wins outright,
// then the largest required set), and when two overloads tie at the top score it
// returns them as competing signatures instead of silently picking one, so the
// caller raises PGRST203 the way PostgREST does for unresolvable overloads.
func (r *StaticRegistry) Resolve(name string, args ArgSet) (*Function, []string, bool) {
	cands := r.byName[name]
	if len(cands) == 0 {
		return nil, nil, false
	}
	var best *Function
	bestScore := -1
	var tied []*Function
	for _, f := range cands {
		if !satisfiable(f, args) {
			continue
		}
		score := len(f.Required())
		if exactMatch(f, args) {
			score += 1000 // an exact parameter-set match wins outright
		}
		switch {
		case score > bestScore:
			best, bestScore, tied = f, score, []*Function{f}
		case score == bestScore:
			tied = append(tied, f)
		}
	}
	if best == nil {
		return nil, nil, false
	}
	if len(tied) > 1 {
		sigs := make([]string, len(tied))
		for i, f := range tied {
			sigs[i] = f.Signature("")
		}
		sort.Strings(sigs)
		return nil, sigs, false
	}
	return best, nil, true
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

// ParseRegistry decodes a JSON function-registry declaration into a
// StaticRegistry ready to Register on a backend. The JSON is an array of
// function objects; each carries:
//
//	name        string           required; bare function name
//	sql         string           required; parameterized SQL with :name placeholders
//	comment     string           optional; surfaces in the OpenAPI document
//	params      []{name, type, optional?, default?}
//	returns     {kind: "scalar"|"setof"|"table", type?, columns?}
//	volatility  "volatile"|"stable"|"immutable"   (default: volatile)
//
// Returns an error when the JSON is malformed; an empty array yields an empty
// registry. Schemas are stripped from names; a name of "api.add" resolves as "add".
func ParseRegistry(rawJSON string) (*StaticRegistry, error) {
	rawJSON = strings.TrimSpace(rawJSON)
	if rawJSON == "" {
		return NewStaticRegistry(nil), nil
	}
	type paramDecl struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Optional bool   `json:"optional"`
		Default  any    `json:"default"`
		Variadic bool   `json:"variadic"`
		RawBody  bool   `json:"rawBody"`
	}
	type returnDecl struct {
		Kind    string `json:"kind"`
		Type    string `json:"type"`
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
	}
	type fnDecl struct {
		Name       string      `json:"name"`
		SQL        string      `json:"sql"`
		Comment    string      `json:"comment"`
		Params     []paramDecl `json:"params"`
		Returns    returnDecl  `json:"returns"`
		Volatility string      `json:"volatility"`
	}
	var decls []fnDecl
	if err := json.Unmarshal([]byte(rawJSON), &decls); err != nil {
		return nil, fmt.Errorf("function-registry: %w", err)
	}
	fns := make([]*Function, 0, len(decls))
	for _, d := range decls {
		// Strip schema prefix (e.g. "api.add" → "add").
		name := d.Name
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		var vol Volatility
		switch strings.ToLower(d.Volatility) {
		case "stable":
			vol = Stable
		case "immutable":
			vol = Immutable
		default:
			vol = Volatile
		}
		params := make([]Param, len(d.Params))
		for i, p := range d.Params {
			params[i] = Param{
				Name:     p.Name,
				Type:     p.Type,
				Optional: p.Optional,
				Default:  p.Default,
				Variadic: p.Variadic,
				RawBody:  p.RawBody,
			}
		}
		var ret ReturnShape
		switch strings.ToLower(d.Returns.Kind) {
		case "void":
			ret.Kind = ReturnVoid
		case "setof":
			ret.Kind = ReturnSetOf
		case "table":
			ret.Kind = ReturnTable
			ret.Columns = make([]Column, len(d.Returns.Columns))
			for i, c := range d.Returns.Columns {
				ret.Columns[i] = Column{Name: c.Name, Type: c.Type}
			}
		default:
			ret.Kind = ReturnScalar
		}
		ret.Type = d.Returns.Type
		fns = append(fns, &Function{
			Name:       name,
			Params:     params,
			Comment:    d.Comment,
			Returns:    ret,
			Volatility: vol,
			Query:      &PortableQuery{SQL: d.SQL},
		})
	}
	return NewStaticRegistry(fns), nil
}

// EmptyRegistry is a registry with no functions; every Lookup misses. A backend
// that has not been given any functions returns this so the frontend raises a
// clean PGRST202 rather than dereferencing nil.
type EmptyRegistry struct{}

// Lookup always misses: an empty registry has no functions.
func (EmptyRegistry) Lookup(string, ArgSet) (*Function, bool) { return nil, false }

// Resolve always misses with no ambiguity: an empty registry has no functions.
func (EmptyRegistry) Resolve(string, ArgSet) (*Function, []string, bool) {
	return nil, nil, false
}

// List returns no functions.
func (EmptyRegistry) List() []*Function { return nil }
