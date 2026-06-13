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
			out = append(out, Relationship{
				Name:    fk.Name,
				Card:    CardToOne,
				Target:  target,
				Local:   fk.Columns,
				Foreign: fk.RefColumns,
				hints:   append([]string{fk.Name}, fk.Columns...),
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
			if isUnique(target, fk.Columns) {
				card = CardToOne
			}
			out = append(out, Relationship{
				Name:    fk.Name,
				Card:    card,
				Target:  target,
				Local:   fk.RefColumns,
				Foreign: fk.Columns,
				hints:   append([]string{fk.Name}, fk.Columns...),
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
					Name:     j.Name,
					Card:     CardToMany,
					Target:   target,
					Local:    toParent.RefColumns,
					Foreign:  toTarget.RefColumns,
					Junction: j,
					JLocal:   toParent.Columns,
					JForeign: toTarget.Columns,
					hints:    junctionHints(j, toTarget),
				})
			}
		}
	}

	return out, true
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
