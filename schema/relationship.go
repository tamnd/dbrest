package schema

import "slices"

// This file adds foreign keys to the model and resolves the embeddable
// relationships the planner needs for resource embedding (spec 09). A
// relationship is derived from foreign-key direction the way PostgREST derives
// it: a foreign key pointing out of the parent is to-one, the reverse view of a
// foreign key pointing in is to-many, and a junction relation with a foreign key
// to each side is a many-to-many. MongoDB and other FK-less engines will declare
// these edges instead; the resolved shape is identical, so the planner and the
// backends do not branch on how the edge was discovered.

// Card is the cardinality of a resolved relationship. It decides whether the
// embedded value renders as a JSON object (to-one) or a JSON array (to-many).
type Card uint8

const (
	CardToOne Card = iota
	CardToMany
)

// ForeignKey is one outgoing foreign key on a relation: the Columns on this
// relation reference RefColumns on RefSchema.RefRelation. Name is the catalog
// constraint name where the engine has one, or a synthesized stable name (SQLite
// foreign keys are unnamed, so the introspector synthesizes {child}_{cols}_fkey).
type ForeignKey struct {
	Name        string
	Columns     []string
	RefSchema   string
	RefRelation string
	RefColumns  []string
	// SourceRelation, when set, is the base relation this foreign key was
	// projected from onto a view. It makes the base table name an extra
	// disambiguation hint for the view's relationship, the third hint kind
	// PostgREST documents for view-sourced edges (spec 09).
	SourceRelation string
}

// hintNames is the set of disambiguation names a derived edge over this foreign
// key exposes: the constraint name, each participating column, and, for a foreign
// key projected onto a view, the base table name.
func (fk *ForeignKey) hintNames() []string {
	hints := append([]string{fk.Name}, fk.Columns...)
	if fk.SourceRelation != "" {
		hints = append(hints, fk.SourceRelation)
	}
	return hints
}

// references reports whether this foreign key points at the given relation.
func (fk *ForeignKey) references(r *Relation) bool {
	return fk.RefRelation == r.Name && fk.RefSchema == r.Schema
}

// Relationship is a resolved embeddable edge from a parent relation to a target.
// Local and Foreign are the join columns on the parent and target sides. For a
// many-to-many edge Junction is non-nil and the join crosses it in two hops:
// parent.Local = Junction.JLocal and Junction.JForeign = target.Foreign.
type Relationship struct {
	Name     string // the edge name a hint can match (FK or junction name)
	Card     Card
	Target   *Relation
	Local    []string
	Foreign  []string
	Junction *Relation
	JLocal   []string
	JForeign []string

	// FuncSchema and FuncName, when set, mark a computed relationship: the edge is
	// not a column join but a call to FuncSchema.FuncName(parent_row), which yields
	// the target rows. Local/Foreign/Junction are unused for such an edge; the
	// compiler renders the function call in the embed's FROM and correlates through
	// the row argument instead of a join predicate (spec 11).
	FuncSchema string
	FuncName   string

	// Cardinality is the four-way spelling PostgREST reports in a PGRST201 details
	// array ("many-to-one", "one-to-one", "one-to-many", "many-to-many"), derived
	// the way upstream derives it: a forward foreign key is many-to-one, or
	// one-to-one when its columns are unique on the parent; a backward foreign key
	// is one-to-many, or one-to-one when unique on the target; a junction edge is
	// many-to-many. Card stays the planner's two-way join shape.
	Cardinality string

	// hints is the set of names a disambiguation hint may match: the edge name
	// and each participating column. Matched case-sensitively, like PostgREST.
	hints []string
}

// MatchesHint reports whether a disambiguation hint (after the `!`) names this
// relationship, by its edge name or by a participating column. PostgREST resolves
// a hint against the relationship name first, then its columns; either match here
// is sufficient because both are folded into the hint set.
func (rel Relationship) MatchesHint(hint string) bool {
	return slices.Contains(rel.hints, hint)
}

// Relationships returns every embeddable edge from parent to the relation named
// targetName, and whether that target relation exists in the model. The planner
// turns the result into the PGRST200 (no relationship) and PGRST201 (ambiguous)
// errors: zero edges with an existing target, or any count with a missing
// target, is PGRST200; more than one surviving edge is PGRST201.
func (m *Model) Relationships(parent *Relation, targetName string, searchPath []string) ([]Relationship, bool) {
	target, ok := m.Lookup(targetName, searchPath)
	if !ok {
		return nil, false
	}

	var out []Relationship

	// Forward: a foreign key on the parent pointing at the target is to-one.
	for _, fk := range parent.ForeignKeys {
		if fk.references(target) {
			card := "many-to-one"
			if isUnique(parent, fk.Columns) {
				card = "one-to-one"
			}
			out = append(out, Relationship{
				Name:        fk.Name,
				Card:        CardToOne,
				Cardinality: card,
				Target:      target,
				Local:       fk.Columns,
				Foreign:     fk.RefColumns,
				hints:       fk.hintNames(),
			})
		}
	}

	// Backward: a foreign key on the target pointing at the parent is the reverse
	// view of the same key. It is to-many in general, but to-one when the FK
	// columns are unique on the target (its primary key or a unique constraint),
	// because then at most one target row references each parent row (spec 09).
	for _, fk := range target.ForeignKeys {
		if fk.references(parent) {
			card := CardToMany
			cardinality := "one-to-many"
			if isUnique(target, fk.Columns) {
				card = CardToOne
				cardinality = "one-to-one"
			}
			out = append(out, Relationship{
				Name:        fk.Name,
				Card:        card,
				Cardinality: cardinality,
				Target:      target,
				Local:       fk.RefColumns,
				Foreign:     fk.Columns,
				hints:       fk.hintNames(),
			})
		}
	}

	// Many-to-many: a junction relation whose foreign keys to the two ends are
	// part of its composite primary key. Every (toParent, toTarget) FK pair is a
	// separate, hintable edge, so two keys to one end make the embed ambiguous
	// rather than silently picking one (spec 09).
	for _, j := range m.Relations() {
		if j == parent || j == target {
			continue
		}
		for _, toParent := range junctionFKs(j, parent) {
			for _, toTarget := range junctionFKs(j, target) {
				if toParent == toTarget {
					continue // a self-to-self junction needs two distinct keys
				}
				out = append(out, Relationship{
					Name:        j.Name,
					Card:        CardToMany,
					Cardinality: "many-to-many",
					Target:      target,
					Local:       toParent.RefColumns,
					Foreign:     toTarget.RefColumns,
					Junction:    j,
					JLocal:      toParent.Columns,
					JForeign:    toTarget.Columns,
					hints:       junctionHints(j, toTarget),
				})
			}
		}
	}

	// Declared and computed edges: relationships supplied outside the catalog. A
	// declared edge whose name equals a derived one overrides it, so a computed
	// relationship can replace an auto-detected edge and a config-declared edge can
	// disambiguate a self-referential FK that derivation leaves ambiguous (spec 09).
	// Function-backed computed relationships (spec 11) join this set and override the
	// same way: an edge named like a derived one replaces it.
	declared := m.declaredEdges(parent, target)
	declared = append(declared, computedRelEdges(parent, target)...)
	if len(declared) > 0 {
		overridden := make(map[string]bool, len(declared))
		for _, d := range declared {
			overridden[d.Name] = true
		}
		kept := out[:0:0]
		for _, e := range out {
			if !overridden[e.Name] {
				kept = append(kept, e)
			}
		}
		kept = append(kept, declared...)
		out = kept
	}

	return out, true
}

// declaredEdges returns the registered declared and computed relationships from
// parent to target as resolved Relationship values, resolving each junction
// relation against the model. An entry whose target or junction is not in the
// model is skipped rather than failing the whole resolution.
func (m *Model) declaredEdges(parent, target *Relation) []Relationship {
	var out []Relationship
	for _, d := range m.declared {
		if d.ParentName != parent.Name || d.ParentSchema != parent.Schema {
			continue
		}
		if d.TargetName != target.Name || d.TargetSchema != target.Schema {
			continue
		}
		rel := Relationship{
			Name:        d.Name,
			Card:        d.Card,
			Cardinality: declaredCardinality(d.Card),
			Target:      target,
			Local:       d.Local,
			Foreign:     d.Foreign,
			hints:       append([]string{d.Name}, d.Hints...),
		}
		if d.JunctionName != "" {
			j, ok := m.Lookup(d.JunctionName, junctionPath(d.JunctionSchema))
			if !ok {
				continue
			}
			rel.Junction = j
			rel.JLocal = d.JLocal
			rel.JForeign = d.JForeign
			rel.Cardinality = "many-to-many"
		}
		out = append(out, rel)
	}
	return out
}

// ComputedRelByName resolves a computed relationship on parent by its edge name
// (the function name), inferring the target relation from the function's return
// type. PostgREST embeds a computed relationship by the function name, which need
// not equal the target relation name, so the planner cannot resolve it by the
// target-name path the way a foreign-key edge resolves. It returns the resolved
// edge and whether a computed relationship of that name exists with its target in
// the model.
func (m *Model) ComputedRelByName(parent *Relation, name string, searchPath []string) (*Relationship, bool) {
	for _, cr := range parent.ComputedRels {
		if cr.Name != name {
			continue
		}
		target, ok := m.Lookup(cr.TargetName, []string{cr.TargetSchema})
		if !ok {
			return nil, false
		}
		return &Relationship{
			Name:        cr.Name,
			Card:        cr.Card,
			Cardinality: declaredCardinality(cr.Card),
			Target:      target,
			FuncSchema:  cr.FuncSchema,
			FuncName:    cr.Name,
			hints:       []string{cr.Name},
		}, true
	}
	return nil, false
}

// computedRelEdges returns the function-backed edges from parent to target: each
// computed relationship on the parent whose target is this relation, as a
// Relationship carrying the function to call instead of join columns. The edge is
// hintable by its name (the function name), matching how a derived edge is
// hintable by its constraint name.
func computedRelEdges(parent, target *Relation) []Relationship {
	var out []Relationship
	for _, cr := range parent.ComputedRels {
		if cr.TargetName != target.Name || cr.TargetSchema != target.Schema {
			continue
		}
		out = append(out, Relationship{
			Name:        cr.Name,
			Card:        cr.Card,
			Cardinality: declaredCardinality(cr.Card),
			Target:      target,
			FuncSchema:  cr.FuncSchema,
			FuncName:    cr.Name,
			hints:       []string{cr.Name},
		})
	}
	return out
}

// junctionPath turns a declared junction schema into a one-element search path,
// or an empty path (direct key match) when the schema is unset.
func junctionPath(schemaName string) []string {
	if schemaName == "" {
		return nil
	}
	return []string{schemaName}
}

// DeclaredRel is a relationship supplied outside the catalog: a config-declared
// edge on an FK-less backend, or an emulated computed relationship. The planner
// resolves it exactly like a derived edge, and it overrides a derived edge of the
// same name (spec 09). Local and Foreign are the parent and target join columns;
// for a many-to-many declared edge JunctionName names the junction relation and
// JLocal/JForeign are its columns on the parent and target sides.
type DeclaredRel struct {
	Name           string
	ParentSchema   string
	ParentName     string
	TargetSchema   string
	TargetName     string
	Card           Card
	Local          []string
	Foreign        []string
	JunctionSchema string
	JunctionName   string
	JLocal         []string
	JForeign       []string
	// Hints are extra names a disambiguation hint may match, beyond the edge name
	// (the participating column names a computed relationship wants hintable).
	Hints []string
}

// declaredCardinality spells a declared edge's two-way Card as the PGRST201
// four-way cardinality. A declared edge carries no parent-side uniqueness or
// direction, so a to-one edge reads as many-to-one and a to-many edge as
// one-to-many; a junction edge is set to many-to-many by the caller.
func declaredCardinality(c Card) string {
	if c == CardToMany {
		return "one-to-many"
	}
	return "many-to-one"
}

// isUnique reports whether cols (as a set) is the relation's primary key or one
// of its unique constraints, the test that makes a referencing FK one-to-one.
func isUnique(r *Relation, cols []string) bool {
	if sameColumnSet(r.PrimaryKey, cols) {
		return true
	}
	for _, u := range r.Unique {
		if sameColumnSet(u, cols) {
			return true
		}
	}
	return false
}

// sameColumnSet reports whether two column-name lists hold the same set,
// ignoring order (constraint membership does not depend on column order).
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// junctionHints is the hint set for a many-to-many edge: the junction name and
// the target-pointing foreign key, by its constraint name and its columns. The
// hint identifies the edge by how the junction reaches the target, which is what
// disambiguates a self-referential junction where both directions share the same
// pair of columns and only the target side differs (PostgREST).
func junctionHints(j *Relation, toTarget *ForeignKey) []string {
	hints := []string{j.Name, toTarget.Name}
	hints = append(hints, toTarget.Columns...)
	return hints
}

// junctionFKs returns the foreign keys on j that point at end and whose columns
// are part of j's primary key, the PostgREST rule for what makes j a junction.
// A table with an FK to a relation but no PK over those columns is an incidental
// referencing table, not a junction, so it yields no edge.
func junctionFKs(j, end *Relation) []*ForeignKey {
	var out []*ForeignKey
	for _, fk := range j.ForeignKeys {
		if fk.references(end) && isSubset(fk.Columns, j.PrimaryKey) {
			out = append(out, fk)
		}
	}
	return out
}

// isSubset reports whether every column in cols appears in set.
func isSubset(cols, set []string) bool {
	if len(cols) == 0 {
		return false
	}
	for _, c := range cols {
		if !slices.Contains(set, c) {
			return false
		}
	}
	return true
}
