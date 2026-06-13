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
	"sort"
	"strconv"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/pgtypes"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// Options carries request-level toggles the planner needs that are not part of
// the query itself. The zero value matches a default PostgREST: aggregates are
// off, so an aggregate select item is rejected with PGRST123 until the
// db-aggregates-enabled option turns it on.
type Options struct {
	// AggregatesEnabled mirrors db-aggregates-enabled. When false, a request using
	// count()/col.sum()/... is rejected with PGRST123; the legacy bare count an
	// embed may carry is exempt and always allowed.
	AggregatesEnabled bool
}

// Read resolves a parsed read query against the model and returns an executable
// plan. searchPath orders the schemas an unqualified relation is looked up in.
//
// Scope: this resolves the base read path (relation + column validation).
// Embeds, aggregates, and JSON paths are validated by their own subsystems as
// they land; a query carrying one is passed through for the compiler to reject
// with a clear PGRST127 rather than being silently accepted here.
func Read(model *schema.Model, q *ir.Query, searchPath []string, opts Options) (*ir.Plan, *pgerr.APIError) {
	rel, ok := model.Lookup(q.Relation.Name, searchPath)
	if !ok {
		return nil, pgerr.ErrUnknownTable(q.Relation.Name)
	}
	// Bind the resolved schema/name back onto the query so the compiler emits a
	// fully qualified, model-validated reference.
	q.Relation = ir.Ref{Schema: rel.Schema, Name: rel.Name}

	if err := validateSelect(rel, q.Select, opts.AggregatesEnabled); err != nil {
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
	if err := resolveEmbeds(model, rel, q, searchPath, opts.AggregatesEnabled); err != nil {
		return nil, err
	}
	// Related-order terms (order=rel(col)) are validated once the embeds they
	// reference are resolved, so the relationship's cardinality is known.
	if err := validateRelatedOrder(rel, q); err != nil {
		return nil, err
	}

	return &ir.Plan{Query: q, Rel: rel, ReadOnly: true}, nil
}

// resolveEmbeds binds every embed of a query against the model: it finds the
// relationship from the parent to the embedded resource, applies a disambiguation
// hint, and recurses into nested embeds. A missing relationship is PGRST200; an
// ambiguous one (more than one surviving edge) is PGRST201. The embed's nested
// select, filters, and ordering are validated against the embedded relation.
func resolveEmbeds(model *schema.Model, parent *schema.Relation, q *ir.Query, searchPath []string, aggEnabled bool) *pgerr.APIError {
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

		if err := validateSelect(rel.Target, emb.Query.Select, aggEnabled); err != nil {
			return err
		}
		if err := validateCond(rel.Target, emb.Query.Where); err != nil {
			return err
		}
		if err := validateOrder(rel.Target, emb.Query.Order); err != nil {
			return err
		}
		if err := resolveEmbeds(model, rel.Target, &emb.Query, searchPath, aggEnabled); err != nil {
			return err
		}
		// A nested related order (tasks.order=projects(id)) references the embed's
		// own sub-embeds, now resolved.
		if err := validateRelatedOrder(rel.Target, &emb.Query); err != nil {
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

	// A write's return=representation projection is a read shape, but PostgREST
	// does not allow aggregates there, so the gate stays closed on this path.
	if err := validateSelect(rel, q.Select, false); err != nil {
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
	if err := resolveEmbeds(model, rel, q, searchPath, false); err != nil {
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
func Call(reg rpc.Registry, model *schema.Model, c *ir.Call, isGet bool, searchPath []string) (*ir.Plan, *pgerr.APIError) {
	// On GET the argument-versus-filter split needs the function's parameter
	// names, which the registry knows by function name (the union across every
	// overload). A query key naming a parameter is an argument; the rest are
	// re-read as filters on a table-valued result. An unknown function is left
	// unpartitioned so resolution raises PGRST202, rather than a stray key being
	// mis-parsed as a filter on a result that does not exist.
	if isGet {
		if params, known := paramNameSet(reg, c.Function.Name); known {
			variadic := variadicNameSet(reg, c.Function.Name)
			if perr := c.PartitionGetArgs(
				func(k string) bool { return params[k] },
				func(k string) bool { return variadic[k] },
			); perr != nil {
				return nil, perr
			}
		}
	}

	args := make(rpc.ArgSet, len(c.Args))
	for name := range c.Args {
		args[name] = true
	}
	activeSchema := ""
	if len(searchPath) > 0 {
		activeSchema = searchPath[0]
	}
	fn, ambiguous, ok := reg.Resolve(c.Function.Name, args)
	if !ok {
		if len(ambiguous) > 0 {
			return nil, pgerr.ErrAmbiguousFunction(ambiguous)
		}
		argNames := sortedArgNames(c.Args)
		return nil, pgerr.ErrNoFunction(activeSchema, c.Function.Name, argNames, nearestSignature(reg, activeSchema, c.Function.Name, args))
	}

	// A read method may only call a read-only function; a write-capable function
	// requires POST so it runs in a read-write transaction.
	if isGet && !fn.Volatility.ReadOnly() {
		return nil, pgerr.ErrMethodNotAllowed(
			"Cannot call a volatile function with GET; use POST")
	}

	// A GET argument arrives as text; validate it against the declared parameter
	// type so an invalid value is the same 22P02 on every backend, the way a read
	// filter is coerced. A POST argument is already typed by the JSON body, and an
	// empty text value stays an empty string rather than becoming NULL.
	if err := coerceCallArgs(fn, c); err != nil {
		return nil, err
	}

	// Post-filters apply to a table return; validate their columns against the
	// declared shape when one is given. A scalar or setof-scalar return carries no
	// columns to filter on, and a table return with no declared columns is
	// validated against the engine result at run time (best effort).
	if err := validateCallFilters(fn, c); err != nil {
		return nil, err
	}

	// A function returning rows of a known relation supports embeds on its result,
	// resolved the same way a table read's embeds are. A call with embeds over a
	// function whose result is not a relation has nothing to embed against.
	if len(c.Embeds) > 0 {
		if err := resolveCallEmbeds(model, fn, c, searchPath); err != nil {
			return nil, err
		}
	}

	c.ReadOnly = fn.Volatility.ReadOnly()
	return &ir.Plan{Call: c, Func: fn, ReadOnly: c.ReadOnly}, nil
}

// returnRelation resolves the relation whose rows a function returns, when its
// return type names one (returns setof clients, returns clients). A scalar,
// setof-scalar, anonymous table(...), or void return names no relation, so its
// result has no relationships to embed against.
func returnRelation(model *schema.Model, fn *rpc.Function, searchPath []string) (*schema.Relation, bool) {
	if model == nil {
		return nil, false
	}
	switch fn.Returns.Kind {
	case rpc.ReturnSetOf, rpc.ReturnScalar:
		if fn.Returns.Type == "" {
			return nil, false
		}
		return model.Lookup(fn.Returns.Type, searchPath)
	default:
		return nil, false
	}
}

// resolveCallEmbeds binds an RPC call's embeds against the function's result
// relation. It mirrors the read path by projecting the call's select/where/order
// onto a synthetic query over that relation, so resolveEmbeds, the embed-filter
// reclassification, and related-order validation all apply unchanged. The
// resolved embeds and any reclassified filter tree are carried back onto the
// call. A function whose result is not a relation cannot be embedded on, which is
// the read path's PGRST200.
func resolveCallEmbeds(model *schema.Model, fn *rpc.Function, c *ir.Call, searchPath []string) *pgerr.APIError {
	retRel, ok := returnRelation(model, fn, searchPath)
	if !ok {
		return pgerr.ErrNoRelationship(fn.Name, c.Embeds[0].Target.Name)
	}
	q := &ir.Query{
		Kind:     ir.Read,
		Relation: ir.Ref{Schema: retRel.Schema, Name: retRel.Name},
		Select:   c.Select,
		Where:    c.Where,
		Order:    c.Order,
		Embeds:   c.Embeds,
	}
	reclassifyEmbedFilters(q)
	if err := resolveEmbeds(model, retRel, q, searchPath, false); err != nil {
		return err
	}
	if err := validateRelatedOrder(retRel, q); err != nil {
		return err
	}
	c.Where = q.Where
	c.Embeds = q.Embeds
	return nil
}

// sortedArgNames returns the call's argument names in a stable order, the list
// PostgREST echoes in a PGRST202 message.
func sortedArgNames(args map[string]ir.Value) []string {
	out := make([]string, 0, len(args))
	for name := range args {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// nearestSignature returns the registered overload of the same name whose
// parameter set is closest to the requested arguments, rendered as a "Perhaps you
// meant to call ..." hint. It returns an empty string when nothing of that name
// is registered, so the caller attaches no hint.
func nearestSignature(reg rpc.Registry, schemaName, name string, args rpc.ArgSet) string {
	var best *rpc.Function
	bestScore := -1
	for _, f := range reg.List() {
		if f.Name != name {
			continue
		}
		score := 0
		for _, p := range f.Params {
			if args[p.Name] {
				score++
			} else {
				score-- // a parameter the call did not supply is a small mismatch
			}
		}
		if score > bestScore {
			best, bestScore = f, score
		}
	}
	if best == nil {
		return ""
	}
	return "Perhaps you meant to call the function " + best.Signature(schemaName)
}

// paramNameSet is the union of parameter names across every overload of a
// function name, and whether the name is registered at all. PostgREST partitions
// a GET call's query keys against this set, independent of which overload
// eventually resolves, so a key naming any overload's parameter is an argument
// rather than a filter. The found flag separates a known parameterless function
// (partition its keys as filters) from an unknown name (leave the keys so
// resolution raises PGRST202).
func paramNameSet(reg rpc.Registry, name string) (set map[string]bool, found bool) {
	set = map[string]bool{}
	for _, f := range reg.List() {
		if f.Name != name {
			continue
		}
		found = true
		for _, p := range f.Params {
			set[p.Name] = true
		}
	}
	return set, found
}

// variadicNameSet is the set of variadic parameter names across every overload of
// a function name, so a GET call can collect that key's repeats into a list.
func variadicNameSet(reg rpc.Registry, name string) map[string]bool {
	set := map[string]bool{}
	for _, f := range reg.List() {
		if f.Name != name {
			continue
		}
		for _, p := range f.Params {
			if p.Variadic {
				set[p.Name] = true
			}
		}
	}
	return set
}

// coerceCallArgs validates each GET text argument against its declared parameter
// type, turning a bad value into the 22P02 the read path raises. A POST argument
// is typed by the JSON body and skipped; an undeclared argument cannot reach here
// because resolution already rejected it. A parameter with no declared type is
// carried through unchanged. A variadic argument validates each collected element.
func coerceCallArgs(fn *rpc.Function, c *ir.Call) *pgerr.APIError {
	for name, v := range c.Args {
		if v.JSON != nil {
			continue // a POST argument, already typed
		}
		p, ok := fn.Param(name)
		if !ok || p.Type == "" {
			continue
		}
		if p.Variadic {
			for _, e := range v.List {
				if err := coerce(p.Type, e); err != nil {
					return err
				}
			}
			continue
		}
		if err := coerce(p.Type, v.Text); err != nil {
			return err
		}
	}
	return nil
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
			return pgerr.ErrUndefinedColumn(col.Path[0])
		}
	}
	if err := validateCallCond(cols, c.Where); err != nil {
		return err
	}
	for _, t := range c.Order {
		if !has(t.Path) {
			return pgerr.ErrUndefinedColumn(t.Path[0])
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
			return pgerr.ErrUndefinedColumn(n.Path[0])
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
			return pgerr.ErrUnknownColumn(c, rel.Name)
		}
	}
	for k := range w.Set {
		if !rel.HasColumn(k) {
			return pgerr.ErrUnknownColumn(k, rel.Name)
		}
	}
	if w.Conflict != nil && len(w.Conflict.Target) == 0 {
		w.Conflict.Target = rel.PrimaryKey
	}
	return nil
}

func validateSelect(rel *schema.Relation, items []ir.SelectItem, aggEnabled bool) *pgerr.APIError {
	for _, it := range items {
		switch v := it.(type) {
		case ir.Column:
			if isStarPath(v.Path) {
				continue
			}
			if err := checkColumn(rel, v.Path); err != nil {
				return err
			}
		case ir.Aggregate:
			// The count()/col.agg() function forms are gated behind
			// db-aggregates-enabled; the legacy bare count an embed carries is exempt.
			if !v.Legacy && !aggEnabled {
				return pgerr.ErrAggregatesDisabled()
			}
			if v.Arg != nil {
				if isStarPath(v.Arg.Path) {
					continue
				}
				if err := checkColumn(rel, v.Arg.Path); err != nil {
					return err
				}
			}
		default:
			// Embed references are checked by resolveEmbeds.
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
		// eq/neq carry the column's canonical type so the compiler binds the
		// literal "true"/"false" as a boolean only when the column is boolean;
		// against a text column the words stay text, matching PostgreSQL's
		// type-driven coercion (item 07.4).
		if (n.Op == ir.OpEq || n.Op == ir.OpNeq) && len(n.Path) == 1 {
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
		// A related-order term (order=rel(col)) addresses a column of an embedded
		// resource, not the parent. It is validated against the resolved embed in
		// validateRelatedOrder, after the embeds are bound.
		if t.Rel != "" {
			continue
		}
		if err := checkColumn(rel, t.Path); err != nil {
			return err
		}
	}
	return nil
}

// validateRelatedOrder checks every order=rel(col) term of a query against its
// resolved embeds: the named relation must be embedded in this request (PGRST108
// otherwise) and must be a to-one relationship (PGRST118 otherwise, since a
// to-many embed gives no single sort key). The embed's own column is then
// validated against the embedded relation. The embeds must already be resolved.
func validateRelatedOrder(parent *schema.Relation, q *ir.Query) *pgerr.APIError {
	for _, t := range q.Order {
		if t.Rel == "" {
			continue
		}
		emb := findEmbedByName(q.Embeds, t.Rel)
		if emb == nil {
			return pgerr.ErrRelatedOrderNotEmbedded(t.Rel)
		}
		if emb.Cardinality != ir.CardToOne {
			return pgerr.ErrRelatedOrderNotToOne(parent.Name, t.Rel)
		}
		if err := checkColumn(emb.Rel.Target, t.Path); err != nil {
			return err
		}
	}
	return nil
}

// findEmbedByName returns the embed an order=rel(col) term refers to, matched by
// the embed's alias when it has one, otherwise its written target name. This is
// the same spelling PostgREST resolves the related-order relation against.
func findEmbedByName(embeds []ir.Embed, name string) *ir.Embed {
	for i := range embeds {
		emb := &embeds[i]
		written := emb.Alias
		if written == "" {
			written = emb.Target.Name
		}
		if written == name {
			return emb
		}
	}
	return nil
}

// checkColumn validates that the base column of a path exists on the relation.
// Only the base (first hop) is checked here; JSON sub-paths are opaque to the
// model and validated when the JSON subsystem lands. A column named in select, a
// filter, or order that does not exist is PostgreSQL's 42703 (the reference
// reaches the server under PostgREST), relation-qualified the way the server
// spells it, not the schema-cache PGRST204 reserved for write payloads.
func checkColumn(rel *schema.Relation, path []string) *pgerr.APIError {
	if len(path) == 0 {
		return nil
	}
	if !rel.HasColumn(path[0]) {
		return pgerr.ErrUndefinedColumn(rel.Name + "." + path[0])
	}
	return nil
}
