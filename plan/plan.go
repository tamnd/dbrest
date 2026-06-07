// Package plan resolves a parsed request against the schema model: it binds the
// target relation, validates that every referenced column exists, and produces
// the ir.Plan a backend executes. Parsing (pure syntax) happens in package ir;
// planning is where names meet the model and the PGRST2xx resolution errors
// arise. See spec 05-query-ir-and-planning.
package plan

import (
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/schema"
)

// Read resolves a parsed read query against the model and returns an executable
// plan. searchPath orders the schemas an unqualified relation is looked up in.
//
// Scope: this resolves the base read path (relation + column validation).
// Embeds, aggregates, and JSON paths are validated by their own subsystems as
// they land; a query carrying one is passed through for the compiler to reject
// with a clear PGRST127 rather than being silently accepted here.
func Read(model *schema.Model, q *ir.Query, searchPath []string) (*ir.Plan, *pgerr.APIError) {
	rel, ok := model.Lookup(q.Relation.Name, searchPath)
	if !ok {
		return nil, pgerr.ErrUnknownTable(q.Relation.Name)
	}
	// Bind the resolved schema/name back onto the query so the compiler emits a
	// fully qualified, model-validated reference.
	q.Relation = ir.Ref{Schema: rel.Schema, Name: rel.Name}

	if err := validateSelect(rel, q.Select); err != nil {
		return nil, err
	}
	if err := validateCond(rel, q.Where); err != nil {
		return nil, err
	}
	if err := validateOrder(rel, q.Order); err != nil {
		return nil, err
	}
	if err := resolveEmbeds(model, rel, q, searchPath); err != nil {
		return nil, err
	}

	return &ir.Plan{Query: q, Rel: rel, ReadOnly: true}, nil
}

// resolveEmbeds binds every embed of a query against the model: it finds the
// relationship from the parent to the embedded resource, applies a disambiguation
// hint, and recurses into nested embeds. A missing relationship is PGRST200; an
// ambiguous one (more than one surviving edge) is PGRST201. The embed's nested
// select, filters, and ordering are validated against the embedded relation.
func resolveEmbeds(model *schema.Model, parent *schema.Relation, q *ir.Query, searchPath []string) *pgerr.APIError {
	for i := range q.Embeds {
		emb := &q.Embeds[i]
		rel, err := resolveOne(model, parent, emb, searchPath)
		if err != nil {
			return err
		}
		emb.Rel = rel
		emb.Cardinality = toCardinality(rel.Card)
		// Bind the embedded relation so the compiler emits a model-validated ref.
		emb.Query.Relation = ir.Ref{Schema: rel.Target.Schema, Name: rel.Target.Name}

		if err := validateSelect(rel.Target, emb.Query.Select); err != nil {
			return err
		}
		if err := validateCond(rel.Target, emb.Query.Where); err != nil {
			return err
		}
		if err := validateOrder(rel.Target, emb.Query.Order); err != nil {
			return err
		}
		if err := resolveEmbeds(model, rel.Target, &emb.Query, searchPath); err != nil {
			return err
		}
	}
	return nil
}

// resolveOne picks the single relationship an embed refers to, applying the
// hint after the `!` when present. The parent name in an error is the embed's
// own parent, matching the PostgREST message.
func resolveOne(model *schema.Model, parent *schema.Relation, emb *ir.Embed, searchPath []string) (*schema.Relationship, *pgerr.APIError) {
	cands, found := model.Relationships(parent, emb.Target.Name, searchPath)
	if !found || len(cands) == 0 {
		return nil, pgerr.ErrNoRelationship(parent.Name, emb.Target.Name)
	}
	if emb.Hint != "" {
		filtered := cands[:0:0]
		for _, c := range cands {
			if c.MatchesHint(emb.Hint) {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}
	switch len(cands) {
	case 0:
		return nil, pgerr.ErrNoRelationship(parent.Name, emb.Target.Name)
	case 1:
		c := cands[0]
		return &c, nil
	default:
		return nil, pgerr.ErrAmbiguousEmbed(parent.Name, emb.Target.Name)
	}
}

// toCardinality maps the schema cardinality to the IR's.
func toCardinality(c schema.Card) ir.Cardinality {
	if c == schema.CardToMany {
		return ir.CardToMany
	}
	return ir.CardToOne
}

// Write resolves a parsed write query (insert/update/upsert/delete) against the
// model and returns an executable plan. It binds the relation, validates the
// filter and returning-projection columns, validates every column named in the
// payload (PGRST204 for an unknown one), and defaults an upsert's conflict
// target to the relation's primary key.
//
// Scope: this resolves the base write path. Embedded writes and computed columns
// arrive with their subsystems; a payload that names a real base column is all
// this path accepts.
func Write(model *schema.Model, q *ir.Query, searchPath []string) (*ir.Plan, *pgerr.APIError) {
	rel, ok := model.Lookup(q.Relation.Name, searchPath)
	if !ok {
		return nil, pgerr.ErrUnknownTable(q.Relation.Name)
	}
	q.Relation = ir.Ref{Schema: rel.Schema, Name: rel.Name}

	if err := validateSelect(rel, q.Select); err != nil {
		return nil, err
	}
	if err := validateCond(rel, q.Where); err != nil {
		return nil, err
	}
	if err := validateWrite(rel, q.Write); err != nil {
		return nil, err
	}

	return &ir.Plan{Query: q, Rel: rel, ReadOnly: false}, nil
}

// validateWrite checks the payload columns against the model and resolves an
// upsert's default conflict target.
func validateWrite(rel *schema.Relation, w *ir.WriteSpec) *pgerr.APIError {
	if w == nil {
		return nil
	}
	// The insert column set (first-row keys or explicit columns=) is what the
	// compiler writes; validating it covers the payload that reaches SQL.
	for _, c := range w.Columns {
		if !rel.HasColumn(c) {
			return pgerr.ErrUnknownColumn(c)
		}
	}
	for k := range w.Set {
		if !rel.HasColumn(k) {
			return pgerr.ErrUnknownColumn(k)
		}
	}
	if w.Conflict != nil && len(w.Conflict.Target) == 0 {
		w.Conflict.Target = rel.PrimaryKey
	}
	return nil
}

func validateSelect(rel *schema.Relation, items []ir.SelectItem) *pgerr.APIError {
	for _, it := range items {
		col, ok := it.(ir.Column)
		if !ok {
			// Aggregates and embeds are checked by their subsystems; leave them.
			continue
		}
		if err := checkColumn(rel, col.Path); err != nil {
			return err
		}
	}
	return nil
}

func validateCond(rel *schema.Relation, c *ir.Cond) *pgerr.APIError {
	if c == nil {
		return nil
	}
	switch n := (*c).(type) {
	case ir.And:
		for i := range n.Kids {
			if err := validateCond(rel, &n.Kids[i]); err != nil {
				return err
			}
		}
	case ir.Or:
		for i := range n.Kids {
			if err := validateCond(rel, &n.Kids[i]); err != nil {
				return err
			}
		}
	case ir.Not:
		return validateCond(rel, &n.Kid)
	case ir.Compare:
		return checkColumn(rel, n.Path)
	}
	return nil
}

func validateOrder(rel *schema.Relation, terms []ir.OrderTerm) *pgerr.APIError {
	for _, t := range terms {
		if err := checkColumn(rel, t.Path); err != nil {
			return err
		}
	}
	return nil
}

// checkColumn validates that the base column of a path exists on the relation.
// Only the base (first hop) is checked here; JSON sub-paths are opaque to the
// model and validated when the JSON subsystem lands.
func checkColumn(rel *schema.Relation, path []string) *pgerr.APIError {
	if len(path) == 0 {
		return nil
	}
	if !rel.HasColumn(path[0]) {
		return pgerr.ErrUnknownColumn(path[0])
	}
	return nil
}
