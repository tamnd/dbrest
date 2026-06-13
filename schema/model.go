// Package schema holds the unified schema model: the engine-independent
// description of relations, columns, keys, and relationships that every
// backend's Introspect produces and the planner resolves names against.
//
// The model is the same shape regardless of engine. A PostgreSQL catalog read,
// a SQLite PRAGMA sweep, and a MongoDB declared-schema all converge here, so the
// frontend never branches on the backend. See spec 08-introspection.
package schema

import (
	"slices"
)

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
	// schemaComments holds the database comment on each exposed schema, the
	// source of the OpenAPI info title and description (first line and rest).
	schemaComments map[string]string
	// declared holds relationships supplied outside the catalog: config-declared
	// edges on an FK-less backend (MongoDB) and emulated computed relationships.
	// The planner treats them like derived edges; a declared edge whose name
	// equals a derived one overrides it (spec 09). Empty on a pure catalog model.
	declared []DeclaredRel
}

// AddDeclaredRelationship registers a relationship that does not come from a
// foreign key: a config-declared edge or an emulated computed relationship. It
// is called during introspection or config load, before the model is published.
// The planner resolves it alongside catalog edges, and it overrides a derived
// edge of the same name, the way a computed relationship overrides an
// auto-detected one in PostgREST (spec 09).
func (m *Model) AddDeclaredRelationship(d DeclaredRel) {
	m.declared = append(m.declared, d)
}

// SetSchemaComment records a schema's database comment. It is called during
// introspection, before the model is published; readers use SchemaComment.
func (m *Model) SetSchemaComment(schemaName, comment string) {
	if m.schemaComments == nil {
		m.schemaComments = make(map[string]string)
	}
	m.schemaComments[schemaName] = comment
}

// SchemaComment returns the database comment on the named schema, or "" when
// none was recorded.
func (m *Model) SchemaComment(schemaName string) string {
	return m.schemaComments[schemaName]
}

// Relation is one table or view in the exposed schema.
type Relation struct {
	Schema string
	Name   string
	Kind   Kind
	// Comment is the database comment on the relation (COMMENT ON TABLE, or
	// the declared-schema equivalent). The OpenAPI generator splits it into
	// the operation summary (first line) and description (rest), as v14 does.
	Comment    string
	Columns    []*Column
	PrimaryKey []string // column names forming the PK, in order; may be empty
	// Unique are the relation's unique constraints, each a set of column names. A
	// foreign key whose columns match the PK or one of these is one-to-one from the
	// referenced side, so the reverse embed renders as an object (spec 09). An
	// engine whose introspector does not read unique constraints leaves this empty.
	Unique [][]string
	// ForeignKeys are the relation's outgoing foreign keys, the raw material the
	// planner resolves embeds from (spec 09). Empty on an engine without them.
	ForeignKeys []*ForeignKey
	// FullText are the relation's full-text indexes, the structure an fts
	// predicate lowers against on engines that need one (spec 21). SQLite fills it
	// from the FTS5 virtual tables that shadow a base table; an engine with
	// column-agnostic full-text (PostgreSQL's tsvector) leaves it empty.
	FullText []*FullTextIndex
	// ViewColumns maps this view's output columns to the base-relation columns
	// they project, when the relation is a view whose definition the introspector
	// resolved to simple base-column references. It is empty for tables and for
	// views the introspector does not project (UNIONs, expression columns). The
	// model projects base-table foreign keys onto the view through it, so a view
	// embeds the way the base table does (spec 09).
	ViewColumns []ViewColumn
	// Computed are the relation's computed fields: functions taking the relation's
	// row type and returning a scalar, exposed as virtual columns the client can
	// select, filter, and order by (PostgREST computed fields). The planner accepts
	// their names where a real column is accepted and the compiler renders each as a
	// function call on the row. Empty for engines or relations without any.
	Computed []ComputedField

	byName     map[string]*Column
	byComputed map[string]*ComputedField
}

// ComputedField is a function-backed virtual column: a function taking the
// relation's row type and returning a scalar. Name is the field as the client
// selects it (the function name); FuncSchema is the schema the function lives in
// (PostgREST requires it to match the relation's schema); Type is the canonical
// return type, surfaced in OpenAPI and used for type-driven coercion.
type ComputedField struct {
	Name       string
	FuncSchema string
	Type       string
}

// ViewColumn records that a view's output column projects one base-relation
// column. The introspector emits these by parsing the view definition; the model
// uses them to carry base-table foreign keys onto the view.
type ViewColumn struct {
	Name         string // the view's output column name
	BaseSchema   string
	BaseRelation string
	BaseColumn   string
}

// Column is one attribute of a relation.
type Column struct {
	Name string
	Type string // canonical PG type name (spec 16)
	// Comment is the database comment on the column. The OpenAPI generator
	// surfaces it on the column's rowFilter parameter and ahead of the pk/fk
	// notes in the definition property, matching v14.
	Comment    string
	Nullable   bool
	HasDefault bool
	// Identity reports whether the column is an auto-generated identity/serial
	// column (IDENTITY on SQL Server, SERIAL/GENERATED ALWAYS AS IDENTITY on
	// PostgreSQL). Backends that support explicit-identity inserts (e.g. SQL
	// Server's IDENTITY_INSERT) use this to decide whether to enable it.
	Identity bool
	// Position is the 1-based ordinal, used for stable ordering.
	Position int
}

// FullTextIndex is an engine-side full-text facility covering one or more of a
// relation's columns. The planner attaches the covering index to an fts predicate
// so the compiler can lower the engine's match form; a backend that requires one
// and finds none raises PGRST127 rather than silently scanning (spec 21).
type FullTextIndex struct {
	// Name is the engine object that answers the match (a SQLite FTS5 virtual
	// table).
	Name string
	// Columns are the base-relation columns the index covers.
	Columns []string
	// RowidColumn is the base column aligned with the index's rowid (FTS5
	// content_rowid); empty means the implicit rowid.
	RowidColumn string
}

// FullTextIndexFor returns the first full-text index covering the named column,
// or nil when none does.
func (r *Relation) FullTextIndexFor(column string) *FullTextIndex {
	for _, idx := range r.FullText {
		if slices.Contains(idx.Columns, column) {
			return idx
		}
	}
	return nil
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
	m.projectViews()
	return m
}

func (r *Relation) index() {
	r.byName = make(map[string]*Column, len(r.Columns))
	for _, c := range r.Columns {
		r.byName[c.Name] = c
	}
	r.byComputed = make(map[string]*ComputedField, len(r.Computed))
	for i := range r.Computed {
		r.byComputed[r.Computed[i].Name] = &r.Computed[i]
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

// ComputedFieldFor returns the named computed field and whether it exists. A
// computed field is selectable, filterable, and orderable like a real column but
// is backed by a function call rather than stored data.
func (r *Relation) ComputedFieldFor(name string) (*ComputedField, bool) {
	c, ok := r.byComputed[name]
	return c, ok
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

// Lookup resolves a relation name against the search path, trying each schema
// in order. Request resolution passes the single active schema (selected by
// the profile headers, defaulting to the first exposed schema), so a request
// can never reach a relation outside it: PostgREST treats the path segment as
// a bare name within the active schema, never as a qualified reference. With
// an empty searchPath the name is matched directly against the model keys,
// the mode introspection-internal callers use.
func (m *Model) Lookup(name string, searchPath []string) (*Relation, bool) {
	if len(searchPath) == 0 {
		r, ok := m.relations[name]
		return r, ok
	}
	for _, s := range searchPath {
		if r, ok := m.relations[Key(s, name)]; ok {
			return r, ok
		}
	}
	return nil, false
}

// RelationsIn returns the relations of one schema in deterministic insertion
// order. It is the per-schema view the OpenAPI root builds its document from,
// so two same-named relations in different schemas can never collide there.
func (m *Model) RelationsIn(schemaName string) []*Relation {
	var out []*Relation
	for _, k := range m.order {
		if r := m.relations[k]; r.Schema == schemaName {
			out = append(out, r)
		}
	}
	return out
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
