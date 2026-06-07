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

	// Backward: a foreign key on the target pointing at the parent is to-many
	// (the reverse view of the same key).
	for _, fk := range target.ForeignKeys {
		if fk.references(parent) {
			out = append(out, Relationship{
				Name:    fk.Name,
				Card:    CardToMany,
				Target:  target,
				Local:   fk.RefColumns,
				Foreign: fk.Columns,
				hints:   append([]string{fk.Name}, fk.Columns...),
			})
		}
	}

	// Many-to-many: a junction relation with a foreign key to each side. The
	// junction is not the parent or the target; its two keys supply the two hops.
	for _, j := range m.Relations() {
		if j == parent || j == target {
			continue
		}
		toParent, toTarget := junctionKeys(j, parent, target)
		if toParent == nil || toTarget == nil {
			continue
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
			hints:    []string{j.Name, toParent.Name, toTarget.Name},
		})
	}

	return out, true
}

// junctionKeys finds the two foreign keys that make j a junction between parent
// and target: one pointing at the parent and a distinct one pointing at the
// target. The distinctness guard matters for a self-referential many-to-many,
// where both keys point at the same relation.
func junctionKeys(j, parent, target *Relation) (toParent, toTarget *ForeignKey) {
	for _, fk := range j.ForeignKeys {
		if toParent == nil && fk.references(parent) {
			toParent = fk
			continue
		}
		if toTarget == nil && fk.references(target) {
			toTarget = fk
		}
	}
	return toParent, toTarget
}
