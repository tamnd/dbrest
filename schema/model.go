// Package schema holds the unified schema model: the engine-independent
// description of relations, columns, keys, and relationships that every
// backend's Introspect produces and the planner resolves names against.
//
// The model is the same shape regardless of engine. A PostgreSQL catalog read,
// a SQLite PRAGMA sweep, and a MongoDB declared-schema all converge here, so the
// frontend never branches on the backend. See spec 08-introspection.
package schema

import "strings"

// Kind distinguishes the relation flavors the planner cares about.
type Kind uint8

const (
	KindTable Kind = iota
	KindView
)

// Model is an immutable snapshot of the exposed schema. The schema cache holds
// one and swaps it atomically on reload; readers never mutate it.
type Model struct {
	// relations is keyed by the lookup name (see Key). Unexported so callers go
	// through Lookup, which applies the exposed-schema and search-path rules.
	relations map[string]*Relation
	// order preserves a deterministic relation order for OpenAPI and tests.
	order []string
}

// Relation is one table or view in the exposed schema.
type Relation struct {
	Schema     string
	Name       string
	Kind       Kind
	Columns    []*Column
	PrimaryKey []string // column names forming the PK, in order; may be empty

	byName map[string]*Column
}

// Column is one attribute of a relation.
type Column struct {
	Name       string
	Type       string // canonical PG type name (spec 16)
	Nullable   bool
	HasDefault bool
	// Position is the 1-based ordinal, used for stable ordering.
	Position int
}

// NewModel builds a Model from a flat relation slice, indexing it for lookup.
func NewModel(rels []*Relation) *Model {
	m := &Model{relations: make(map[string]*Relation, len(rels))}
	for _, r := range rels {
		r.index()
		key := Key(r.Schema, r.Name)
		if _, dup := m.relations[key]; !dup {
			m.order = append(m.order, key)
		}
		m.relations[key] = r
	}
	return m
}

func (r *Relation) index() {
	r.byName = make(map[string]*Column, len(r.Columns))
	for _, c := range r.Columns {
		r.byName[c.Name] = c
	}
}

// Column returns the named column and whether it exists.
func (r *Relation) Column(name string) (*Column, bool) {
	c, ok := r.byName[name]
	return c, ok
}

// HasColumn reports whether the relation exposes the named column.
func (r *Relation) HasColumn(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// ColumnNames returns the column names in ordinal order. It is the whole-row
// projection a write returns when the client asks for the representation but
// names no explicit columns.
func (r *Relation) ColumnNames() []string {
	out := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		out[i] = c.Name
	}
	return out
}

// Key is the canonical map key for a relation. Names are matched
// case-sensitively, matching PostgreSQL's quoted-identifier behavior; an
// unqualified request resolves against the first exposed schema via Lookup.
func Key(schemaName, rel string) string {
	if schemaName == "" {
		return rel
	}
	return schemaName + "." + rel
}

// Lookup resolves a possibly-qualified relation name. An unqualified name
// (no dot) is matched first directly, then against each schema in searchPath in
// order, mirroring PostgREST's exposed-schema / search-path resolution.
func (m *Model) Lookup(name string, searchPath []string) (*Relation, bool) {
	if r, ok := m.relations[name]; ok {
		return r, ok
	}
	if !strings.Contains(name, ".") {
		for _, s := range searchPath {
			if r, ok := m.relations[Key(s, name)]; ok {
				return r, ok
			}
		}
	}
	return nil, false
}

// Relations returns the relations in deterministic insertion order.
func (m *Model) Relations() []*Relation {
	out := make([]*Relation, 0, len(m.order))
	for _, k := range m.order {
		out = append(out, m.relations[k])
	}
	return out
}

// Len reports the number of relations in the model.
func (m *Model) Len() int { return len(m.relations) }
