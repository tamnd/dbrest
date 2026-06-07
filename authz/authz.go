// Package authz enforces PostgREST's authorization model on the backends whose
// engine has no row security of its own (spec 14). PostgREST delegates
// authorization to PostgreSQL: GRANT/REVOKE decide which tables and columns a
// role may touch, and Row Level Security policies decide which rows. On
// PostgreSQL dbrest configures the engine and steps back; on SQLite (and the
// other emulated backends) there is no engine row security, so this package
// reproduces the observable result itself, in the frontend, before the query is
// lowered.
//
// For the emulated backends this package is the security boundary, so its checks
// are unbypassable by construction: the privilege gate runs on every request and
// the policy predicate is AND-ed at the top of the filter tree, above every
// client filter, so a client cannot OR its way past it.
//
// The package imports only the IR, the schema model, the request context, and
// the error envelope. Nothing imports it back; the server calls Authorize
// between planning and execution.
package authz

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// Action is a privilege verb a role may be granted on a relation.
type Action uint8

const (
	Select Action = iota
	Insert
	Update
	Delete
)

// Grant is one privilege: a role may perform an action on a relation, optionally
// narrowed to a column set. An empty Columns grants every column.
type Grant struct {
	Role     string
	Relation string
	Action   Action
	Columns  []string
}

// Op is the comparison in a policy term. Equality covers the canonical
// tenant-isolation policy; inequality is its complement. Richer operators are a
// later slice and are intentionally not modeled yet.
type Op uint8

const (
	OpEq Op = iota
	OpNeq
)

// Term is one comparison in a policy predicate: a relation column compared to a
// value that is either a literal or a reference to a request claim. The claim is
// resolved to a literal in the frontend, so the backend never sees the token.
type Term struct {
	Column  string
	Op      Op
	Claim   string // dotted claim path when the value is a claim reference
	Literal any    // literal value when Claim is empty
}

// Predicate is a conjunction of terms. An empty predicate is always true.
type Predicate struct {
	Terms []Term
}

// Policy is a Row Level Security policy for a role on a relation: Using
// constrains which existing rows a read sees and a write may touch, WithCheck
// constrains the row values a write may produce.
type Policy struct {
	Relation  string
	Role      string
	Using     Predicate
	WithCheck Predicate
}

// Registry holds the privilege grants and RLS policies the gate consults. It is
// built once and is read-only thereafter, so it is safe for concurrent use.
type Registry struct {
	grants   map[grantKey]colSet
	policies map[polKey]*Policy
}

type grantKey struct {
	role   string
	rel    string
	action Action
}

type polKey struct {
	role string
	rel  string
}

// colSet is the merged column grant for a (role, relation, action): all is true
// when any matching grant covers every column, otherwise cols is the union.
type colSet struct {
	all  bool
	cols map[string]bool
}

// NewRegistry builds a registry from a flat list of grants and policies, merging
// grants that share a (role, relation, action) and AND-ing policy predicates
// that share a (role, relation).
func NewRegistry(grants []Grant, policies []Policy) *Registry {
	r := &Registry{
		grants:   map[grantKey]colSet{},
		policies: map[polKey]*Policy{},
	}
	for _, g := range grants {
		k := grantKey{g.Role, g.Relation, g.Action}
		cur, ok := r.grants[k]
		if !ok {
			cur = colSet{cols: map[string]bool{}}
		}
		if len(g.Columns) == 0 {
			cur.all = true
		}
		for _, c := range g.Columns {
			cur.cols[c] = true
		}
		r.grants[k] = cur
	}
	for i := range policies {
		p := policies[i]
		k := polKey{p.Role, p.Relation}
		if cur, ok := r.policies[k]; ok {
			cur.Using.Terms = append(cur.Using.Terms, p.Using.Terms...)
			cur.WithCheck.Terms = append(cur.WithCheck.Terms, p.WithCheck.Terms...)
			continue
		}
		cp := p
		r.policies[k] = &cp
	}
	return r
}

// Authorize gates a planned query for the request's role and injects any RLS
// policy. It runs after planning and before execution, mutating the plan's query
// in place: it rejects a request the role may not make, narrows or rejects a
// projection by column privilege, and AND-s the policy predicate onto the filter
// tree. An RPC plan carries no relation query and is passed through unchanged.
func (r *Registry) Authorize(rc *reqctx.Context, p *ir.Plan) *pgerr.APIError {
	if p == nil || p.Query == nil {
		return nil
	}
	q := p.Query
	rel := q.Relation.Name
	role := rc.Role

	// The action gate: every action the request performs must be granted.
	for _, a := range actionsFor(q.Kind) {
		if _, ok := r.grants[grantKey{role, rel, a}]; !ok {
			return pgerr.ErrPermissionDenied(rel, rc.Anonymous)
		}
	}

	// The column gate. A read always projects; a write projects only when it
	// returns the representation. Either way the projection is gated against the
	// SELECT column grant, and a star/empty projection is narrowed to it.
	if q.Kind == ir.Read || (q.Write != nil && q.Write.Return == ir.ReturnRepresentation) {
		if err := r.gateSelect(role, rel, q, p.Rel, rc.Anonymous); err != nil {
			return err
		}
	}

	// The write-column gate: a payload column must be within the write action's
	// column grant.
	if q.Write != nil {
		for _, a := range writeColumnActions(q.Kind) {
			if err := r.gateWriteColumns(role, rel, q.Write, a, rc.Anonymous); err != nil {
				return err
			}
		}
	}

	// Row Level Security: inject the USING predicate and validate WITH CHECK.
	pol, ok := r.policies[polKey{role, rel}]
	if !ok {
		return nil
	}
	switch q.Kind {
	case ir.Read, ir.Update, ir.Delete, ir.Upsert:
		injectWhere(q, usingConds(pol.Using, rc.Claims))
	}
	switch q.Kind {
	case ir.Insert, ir.Upsert:
		for _, row := range q.Write.Rows {
			if !evalPredicate(pol.WithCheck, rc.Claims, rowLookup(row), false) {
				return pgerr.ErrRLSViolation(rel)
			}
		}
	case ir.Update:
		// Only the changed columns are validated here; a column the update does
		// not set keeps its existing value, already constrained by the USING
		// predicate injected above.
		if !evalPredicate(pol.WithCheck, rc.Claims, rowLookup(q.Write.Set), true) {
			return pgerr.ErrRLSViolation(rel)
		}
	}
	return nil
}

// gateSelect enforces the SELECT column grant on a projection. An explicitly
// named forbidden column rejects the request; a star or empty projection is
// narrowed to the granted columns, matching how PostgreSQL drops what a role may
// not read while refusing an explicit ungranted column.
func (r *Registry) gateSelect(role, rel string, q *ir.Query, relSchema *schema.Relation, anon bool) *pgerr.APIError {
	g, ok := r.grants[grantKey{role, rel, Select}]
	if !ok {
		return pgerr.ErrPermissionDenied(rel, anon)
	}
	if g.all {
		return nil
	}
	hasStar := len(q.Select) == 0
	for _, it := range q.Select {
		c, isCol := it.(ir.Column)
		if !isCol {
			continue
		}
		if isStar(c) {
			hasStar = true
			continue
		}
		if !g.cols[baseColumn(c)] {
			return pgerr.ErrPermissionDenied(rel, anon)
		}
	}
	if hasStar && relSchema != nil {
		q.Select = narrowProjection(q.Select, g.cols, relSchema)
	}
	return nil
}

// gateWriteColumns enforces the write action's column grant against the payload
// columns (the insert column list and the update SET keys).
func (r *Registry) gateWriteColumns(role, rel string, w *ir.WriteSpec, action Action, anon bool) *pgerr.APIError {
	g, ok := r.grants[grantKey{role, rel, action}]
	if !ok {
		return pgerr.ErrPermissionDenied(rel, anon)
	}
	if g.all {
		return nil
	}
	for _, c := range w.Columns {
		if !g.cols[c] {
			return pgerr.ErrPermissionDenied(rel, anon)
		}
	}
	for k := range w.Set {
		if !g.cols[k] {
			return pgerr.ErrPermissionDenied(rel, anon)
		}
	}
	return nil
}

// narrowProjection rewrites a star or empty projection to the granted columns in
// relation order, keeping any embed references and any already-allowed explicit
// columns, with duplicates removed.
func narrowProjection(items []ir.SelectItem, allowed map[string]bool, rel *schema.Relation) []ir.SelectItem {
	out := make([]ir.SelectItem, 0, len(rel.Columns))
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] || !allowed[name] {
			return
		}
		seen[name] = true
		out = append(out, ir.Column{Path: []string{name}})
	}
	for _, c := range rel.Columns {
		add(c.Name)
	}
	for _, it := range items {
		switch v := it.(type) {
		case ir.Column:
			if !isStar(v) {
				add(baseColumn(v))
			}
		default:
			out = append(out, it)
		}
	}
	return out
}

// usingConds turns a USING predicate into IR conditions with each claim resolved
// to a literal. A term whose claim is missing becomes an always-false condition
// (an empty IN), so an absent claim denies every row rather than leaking them.
func usingConds(p Predicate, claims map[string]any) []ir.Cond {
	conds := make([]ir.Cond, 0, len(p.Terms))
	for _, t := range p.Terms {
		rhs, ok := resolveRHS(t, claims)
		if !ok {
			conds = append(conds, ir.Compare{Path: []string{t.Column}, Op: ir.OpIn, Value: ir.Value{List: nil}})
			continue
		}
		op := ir.OpEq
		if t.Op == OpNeq {
			op = ir.OpNeq
		}
		conds = append(conds, ir.Compare{Path: []string{t.Column}, Op: op, Value: ir.Value{Text: rhs}})
	}
	return conds
}

// injectWhere AND-s the injected conditions onto the top of the query's filter
// tree. The client's entire existing filter becomes a single child of the new
// top-level And, so the client cannot escape the policy by OR-ing its own
// predicate.
func injectWhere(q *ir.Query, conds []ir.Cond) {
	if len(conds) == 0 {
		return
	}
	kids := make([]ir.Cond, 0, len(conds)+1)
	if q.Where != nil {
		kids = append(kids, *q.Where)
	}
	kids = append(kids, conds...)
	var top ir.Cond = ir.And{Kids: kids}
	q.Where = &top
}

// evalPredicate evaluates a WITH CHECK predicate against a row's new values. A
// term whose column is absent fails the predicate, unless skipAbsent is set (an
// update does not constrain a column it leaves unchanged). A missing claim fails.
func evalPredicate(p Predicate, claims map[string]any, val func(string) (string, bool), skipAbsent bool) bool {
	for _, t := range p.Terms {
		cv, present := val(t.Column)
		if !present {
			if skipAbsent {
				continue
			}
			return false
		}
		rhs, ok := resolveRHS(t, claims)
		if !ok {
			return false
		}
		switch t.Op {
		case OpEq:
			if cv != rhs {
				return false
			}
		case OpNeq:
			if cv == rhs {
				return false
			}
		}
	}
	return true
}

// rowLookup adapts a payload row to the lookup evalPredicate expects.
func rowLookup(row map[string]ir.Value) func(string) (string, bool) {
	return func(col string) (string, bool) {
		v, ok := row[col]
		if !ok {
			return "", false
		}
		return valueToString(v), true
	}
}

// resolveRHS resolves a term's right-hand side to a string: a literal directly,
// or a claim looked up in the request claims. The bool is false when a referenced
// claim is absent.
func resolveRHS(t Term, claims map[string]any) (string, bool) {
	if t.Claim == "" {
		return fmt.Sprint(t.Literal), true
	}
	v, ok := lookupClaim(claims, t.Claim)
	if !ok {
		return "", false
	}
	return scalarString(v), true
}

// lookupClaim walks a dotted claim path through the claims map.
func lookupClaim(claims map[string]any, path string) (any, bool) {
	var cur any = claims
	for seg := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// valueToString renders a payload value as the text used for comparison.
func valueToString(v ir.Value) string {
	if v.JSON != nil {
		return scalarString(v.JSON)
	}
	return v.Text
}

// scalarString renders a JSON scalar (string, number, bool) as plain text, so an
// integer claim like 42 compares as "42" rather than "42.000000".
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// actionsFor lists the privilege actions a query of this kind performs.
func actionsFor(kind ir.QueryKind) []Action {
	switch kind {
	case ir.Read:
		return []Action{Select}
	case ir.Insert:
		return []Action{Insert}
	case ir.Update:
		return []Action{Update}
	case ir.Delete:
		return []Action{Delete}
	case ir.Upsert:
		return []Action{Insert, Update}
	default:
		return nil
	}
}

// writeColumnActions lists the actions whose column grant gates a write payload.
func writeColumnActions(kind ir.QueryKind) []Action {
	switch kind {
	case ir.Insert:
		return []Action{Insert}
	case ir.Update:
		return []Action{Update}
	case ir.Upsert:
		return []Action{Insert, Update}
	default:
		return nil
	}
}

// baseColumn is the base column name a select item projects (its first path
// element, before any JSON sub-path or cast).
func baseColumn(c ir.Column) string {
	if len(c.Path) == 0 {
		return ""
	}
	return c.Path[0]
}

// isStar reports whether a select item is the bare * projection.
func isStar(c ir.Column) bool {
	return len(c.Path) == 1 && c.Path[0] == "*" && c.Cast == "" && c.Last == ir.JSONNone
}
