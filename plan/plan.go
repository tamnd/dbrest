// Package plan resolves a parsed request against the schema model: it binds the
// target relation, validates that every referenced column exists, and produces
// the ir.Plan a backend executes. Parsing (pure syntax) happens in package ir;
// planning is where names meet the model and the PGRST2xx resolution errors
// arise. See spec 05-query-ir-and-planning.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/pgtypes"
	"github.com/tamnd/dbrest/rpc"
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
	// A filter naming an embed (films?actors=not.is.null) is an existence test on
	// the relationship, not a parent column. Reclassify those before column
	// validation so they are not rejected as unknown columns, then validate the
	// rest of the tree. See item 01.12.
	reclassifyEmbedFilters(q)
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
	// A return=representation body is shaped by the same select/embeds a read
	// uses, so resolve the embeds against the target relation here. An unknown or
	// ambiguous relationship is the read path's PGRST200/201 rather than being
	// silently dropped from the response. See item 01.19.
	if err := resolveEmbeds(model, rel, q, searchPath); err != nil {
		return nil, err
	}
	if q.IsPut {
		if err := validatePut(rel, q); err != nil {
			return nil, err
		}
	}

	return &ir.Plan{Query: q, Rel: rel, ReadOnly: false}, nil
}

// validatePut enforces PostgREST's PUT contract before any write: the URL
// filters must be exactly the relation's primary key columns, each with eq
// (PGRST105); no limit or offset may be present (PGRST114); and the body must
// be a single object whose primary key values equal the URL's (PGRST115). A PUT
// addresses one row by its whole key, so anything looser is rejected here rather
// than writing the wrong row.
func validatePut(rel *schema.Relation, q *ir.Query) *pgerr.APIError {
	if q.Limit != nil || q.Offset != nil {
		return pgerr.ErrPutLimit()
	}
	eqs, ok := putEqFilters(q.Where)
	if !ok {
		return pgerr.ErrPutPrimaryKey()
	}
	pk := rel.PrimaryKey
	if len(pk) == 0 || len(eqs) != len(pk) {
		return pgerr.ErrPutPrimaryKey()
	}
	for _, c := range pk {
		if _, ok := eqs[c]; !ok {
			return pgerr.ErrPutPrimaryKey()
		}
	}
	w := q.Write
	if w == nil || len(w.Rows) != 1 {
		return pgerr.ErrPutPayloadKey()
	}
	row := w.Rows[0]
	for _, c := range pk {
		v, ok := row[c]
		if !ok || !putKeyMatches(rel, c, v, eqs[c]) {
			return pgerr.ErrPutPayloadKey()
		}
	}
	return nil
}

// putEqFilters flattens a PUT's WHERE into a map of column to operand text,
// accepting only a conjunction of single-column, non-negated, unquantified eq
// comparisons. It returns ok=false for any other shape (a non-eq operator, an
// or/not tree, a JSON path, or a quantifier), none of which a PUT may carry.
func putEqFilters(c *ir.Cond) (map[string]string, bool) {
	out := map[string]string{}
	var walk func(n ir.Cond) bool
	walk = func(n ir.Cond) bool {
		switch v := n.(type) {
		case ir.And:
			for _, k := range v.Kids {
				if !walk(k) {
					return false
				}
			}
			return true
		case ir.Compare:
			if v.Op != ir.OpEq || len(v.Path) != 1 || v.Quant != ir.QNone || v.Negate {
				return false
			}
			out[v.Path[0]] = v.Value.Text
			return true
		default:
			return false
		}
	}
	if c == nil {
		return out, true
	}
	return out, walk(*c)
}

// putKeyMatches reports whether a payload value for a primary key column equals
// the URL filter text. Both sides are coerced through the column's type so 1 and
// "1" agree; if the type is unknown or either side fails to coerce, the raw text
// forms are compared.
func putKeyMatches(rel *schema.Relation, col string, payload ir.Value, urlText string) bool {
	pj := jsonScalarText(payload.JSON)
	if c, ok := rel.Column(col); ok && c.Type != "" {
		pv, perr := pgtypes.ParseScalar(c.Type, pj)
		uv, uerr := pgtypes.ParseScalar(c.Type, urlText)
		if perr == nil && uerr == nil {
			return fmt.Sprint(pv) == fmt.Sprint(uv)
		}
	}
	return pj == urlText
}

// jsonScalarText renders a decoded JSON scalar as the text PostgREST would
// compare against a URL operand. A JSON number prints without a trailing zero so
// 1 stays "1", not "1.000000".
func jsonScalarText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// Call resolves a parsed RPC call against the function registry and returns an
// executable plan. It selects the overload the argument set satisfies (PGRST202
// when none does), enforces the volatility-versus-method rule (a GET to a
// volatile function is 405), and validates a post-filter select/where/order
// against a table return's declared columns. The resolved function and the
// read-only decision travel on the plan for the backend to lower. See spec 12.
func Call(reg rpc.Registry, c *ir.Call, isGet bool, searchPath []string) (*ir.Plan, *pgerr.APIError) {
	args := make(rpc.ArgSet, len(c.Args))
	for name := range c.Args {
		args[name] = true
	}
	fn, ok := reg.Lookup(c.Function.Name, args)
	if !ok {
		return nil, pgerr.ErrNoFunction(c.Function.Name)
	}

	// A read method may only call a read-only function; a write-capable function
	// requires POST so it runs in a read-write transaction.
	if isGet && !fn.Volatility.ReadOnly() {
		return nil, pgerr.ErrMethodNotAllowed(
			"Cannot call a volatile function with GET; use POST")
	}

	// Post-filters apply to a table return; validate their columns against the
	// declared shape when one is given. A scalar or setof-scalar return carries no
	// columns to filter on, and a table return with no declared columns is
	// validated against the engine result at run time (best effort).
	if err := validateCallFilters(fn, c); err != nil {
		return nil, err
	}

	c.ReadOnly = fn.Volatility.ReadOnly()
	return &ir.Plan{Call: c, Func: fn, ReadOnly: c.ReadOnly}, nil
}

// validateCallFilters checks an RPC call's post-filter columns against a table
// return's declared columns. It is a no-op for scalar and setof-scalar returns
// and for a table return whose columns are not declared.
func validateCallFilters(fn *rpc.Function, c *ir.Call) *pgerr.APIError {
	if fn.Returns.Kind != rpc.ReturnTable || len(fn.Returns.Columns) == 0 {
		return nil
	}
	cols := make(map[string]bool, len(fn.Returns.Columns))
	for _, col := range fn.Returns.Columns {
		cols[col.Name] = true
	}
	has := func(path []string) bool { return len(path) == 0 || cols[path[0]] }
	for _, it := range c.Select {
		col, ok := it.(ir.Column)
		if !ok {
			continue
		}
		if isStarPath(col.Path) {
			continue
		}
		if !has(col.Path) {
			return pgerr.ErrUnknownColumn(col.Path[0])
		}
	}
	if err := validateCallCond(cols, c.Where); err != nil {
		return err
	}
	for _, t := range c.Order {
		if !has(t.Path) {
			return pgerr.ErrUnknownColumn(t.Path[0])
		}
	}
	return nil
}

// isStarPath reports whether a select path is the bare "*".
func isStarPath(path []string) bool { return len(path) == 1 && path[0] == "*" }

// validateCallCond validates the columns of an RPC post-filter tree against the
// table return's column set.
func validateCallCond(cols map[string]bool, c *ir.Cond) *pgerr.APIError {
	if c == nil {
		return nil
	}
	switch n := (*c).(type) {
	case ir.And:
		for i := range n.Kids {
			if err := validateCallCond(cols, &n.Kids[i]); err != nil {
				return err
			}
		}
	case ir.Or:
		for i := range n.Kids {
			if err := validateCallCond(cols, &n.Kids[i]); err != nil {
				return err
			}
		}
	case ir.Not:
		return validateCallCond(cols, &n.Kid)
	case ir.Compare:
		if len(n.Path) > 0 && !cols[n.Path[0]] {
			return pgerr.ErrUnknownColumn(n.Path[0])
		}
	}
	return nil
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
		if isStarPath(col.Path) {
			continue
		}
		if err := checkColumn(rel, col.Path); err != nil {
			return err
		}
	}
	return nil
}

// reclassifyEmbedFilters rewrites, in place, every Compare in the query's filter
// tree whose single-segment path names an embed's OutKey and whose operator is
// `is null` into an ir.EmbedPredicate. PostgREST reads films?actors=not.is.null
// as a semi-join on the actors relationship and films?actors=is.null as an
// anti-join, both usable inside or=(...); without this rewrite the embed name
// would be validated as a parent column and rejected. not.is.null carries the
// Compare's Negate, which becomes Exists (the parent must have a matching row).
// See item 01.12.
func reclassifyEmbedFilters(q *ir.Query) {
	if q.Where == nil || len(q.Embeds) == 0 {
		return
	}
	idx := make(map[string]int, len(q.Embeds))
	for i := range q.Embeds {
		idx[q.Embeds[i].OutKey] = i
	}
	var rw func(c ir.Cond) ir.Cond
	rw = func(c ir.Cond) ir.Cond {
		switch n := c.(type) {
		case ir.And:
			for i := range n.Kids {
				n.Kids[i] = rw(n.Kids[i])
			}
			return n
		case ir.Or:
			for i := range n.Kids {
				n.Kids[i] = rw(n.Kids[i])
			}
			return n
		case ir.Not:
			n.Kid = rw(n.Kid)
			return n
		case ir.Compare:
			if n.Op == ir.OpIs && n.Value.Text == "null" && len(n.Path) == 1 {
				if i, ok := idx[n.Path[0]]; ok {
					return ir.EmbedPredicate{Index: i, Exists: n.Negate}
				}
			}
			return n
		default:
			return c
		}
	}
	nc := rw(*q.Where)
	q.Where = &nc
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
		if err := checkColumn(rel, n.Path); err != nil {
			return err
		}
		if err := checkOperand(rel, n); err != nil {
			return err
		}
		// An fts predicate carries its covering full-text index, when the schema has
		// one for the column, so the compiler can lower the engine's match form. A
		// nil index is left for the backend to interpret: an engine with
		// column-agnostic full-text (PostgreSQL) ignores it, one that needs a
		// structure (SQLite's FTS5) raises PGRST127. See spec 21.
		if n.Op == ir.OpFTS && len(n.Path) == 1 {
			n.FullText = rel.FullTextIndexFor(n.Path[0])
			*c = n
		}
		// Array operators carry the column's canonical type so the dialect can
		// decide whether the column supports array semantics (e.g. SQLite's
		// json_each only applies to JSON-typed columns). See spec 21.
		if (n.Op == ir.OpContains || n.Op == ir.OpContained || n.Op == ir.OpOverlap) && len(n.Path) == 1 {
			if col, ok := rel.Column(n.Path[0]); ok {
				n.ColumnType = col.Type
				*c = n
			}
		}
	}
	return nil
}

// checkOperand coerces a comparison's operand against the column's canonical type
// so a value the type cannot hold (a word where an int4 is wanted) is a clean 400
// in the frontend, identical on every backend, instead of an engine-specific
// error or, worse, a silent affinity coercion. Only the value-bearing operators
// are coerced: pattern operators (like/match/fts) take a pattern, is takes a
// null/boolean keyword, and a base column with a JSON sub-path is opaque to the
// model, so all of those are left alone. See spec 16.
func checkOperand(rel *schema.Relation, c ir.Compare) *pgerr.APIError {
	if len(c.Path) != 1 {
		return nil
	}
	col, ok := rel.Column(c.Path[0])
	if !ok {
		return nil
	}
	// A quantified comparison (eq/gt/gte/lt/lte over a {…} list) carries its
	// operands in the list; coerce each against the column type. Quantified
	// pattern operators (like/ilike/match/imatch) take patterns, not typed values,
	// and are left alone (item 01.1).
	if c.Quant != ir.QNone {
		switch c.Op {
		case ir.OpEq, ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte:
			for _, v := range c.Value.List {
				if err := coerce(col.Type, v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	switch c.Op {
	case ir.OpEq, ir.OpNeq, ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte:
		return coerce(col.Type, c.Value.Text)
	case ir.OpIn:
		for _, v := range c.Value.List {
			if err := coerce(col.Type, v); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// coerce runs the operand through the type's parser, turning a coercion failure
// into the PostgREST 22P02 envelope with the canonical type named.
func coerce(canonicalType, text string) *pgerr.APIError {
	if _, err := pgtypes.ParseScalar(canonicalType, text); err != nil {
		if ce, ok := errors.AsType[*pgtypes.CoerceError](err); ok {
			return pgerr.ErrInvalidInput(ce.Canonical, ce.Input)
		}
		return pgerr.ErrInvalidInput(canonicalType, text)
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
