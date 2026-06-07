package sqlgen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
)

// Statement is a compiled, parameterized SQL statement and its argument list, in
// placeholder order.
type Statement struct {
	SQL  string
	Args []any
}

// builder accumulates SQL text and the matching argument list, handing out
// placeholders through the dialect so a value is never interpolated.
//
// qual is the table qualifier prefixed onto bare column references; it is empty
// for a plain query and set to a table alias while compiling an embedded
// subquery, so the same column writers serve both. aliasN names the embed table
// aliases (t1, t2, ...) deterministically.
type builder struct {
	d      Dialect
	sb     *strings.Builder
	args   []any
	qual   string
	aliasN int
}

// newBuilder starts a builder with an empty output buffer.
func newBuilder(d Dialect) *builder {
	return &builder{d: d, sb: &strings.Builder{}}
}

// capture redirects output to a fresh buffer while f runs, returning what f
// wrote as a string. Argument binding is unaffected: placeholders keep counting
// up on the shared list, so a captured fragment can be spliced into the SQL text
// later and its placeholders still line up. This is how an embedded subquery is
// rendered to a string for nesting inside a JSON object. See spec 09.
func (b *builder) capture(f func() *pgerr.APIError) (string, *pgerr.APIError) {
	saved := b.sb
	b.sb = &strings.Builder{}
	err := f()
	out := b.sb.String()
	b.sb = saved
	return out, err
}

// bind appends a value to the argument list and returns its placeholder.
func (b *builder) bind(v any) string {
	b.args = append(b.args, v)
	return b.d.Placeholder(len(b.args))
}

// colRef renders a column reference, qualified by the current table alias when
// one is set (inside an embed subquery) and bare otherwise.
func (b *builder) colRef(name string) string {
	if b.qual == "" {
		return b.d.QuoteIdent(name)
	}
	return b.qual + "." + b.d.QuoteIdent(name)
}

// CompileRead lowers a resolved read query to a row-returning SELECT. The result
// is a parameterized statement the backend hands to the driver; the renderer
// shapes the returned rows into the response document.
//
// Scope: this compiler covers the base read path (column projection, horizontal
// filters, ordering, pagination). JSON-path projection, aggregates, and resource
// embedding are separate subsystems and report a clear error here until they
// land, rather than silently emitting wrong SQL.
func CompileRead(d Dialect, q *ir.Query) (*Statement, *pgerr.APIError) {
	if len(q.Embeds) > 0 {
		return compileReadEmbedded(d, q)
	}
	b := newBuilder(d)
	b.sb.WriteString("SELECT ")

	if err := b.writeSelect(q.Select); err != nil {
		return nil, err
	}

	b.sb.WriteString(" FROM ")
	b.sb.WriteString(b.qualify(q.Relation))

	if q.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*q.Where); err != nil {
			return nil, err
		}
	}

	hasOrder := len(q.Order) > 0
	if hasOrder {
		if err := b.writeOrder(q.Order); err != nil {
			return nil, err
		}
	}

	if clause := b.d.LimitOffset(q.Limit, q.Offset, hasOrder); clause != "" {
		b.sb.WriteString(" ")
		b.sb.WriteString(clause)
	}

	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// CompileCount lowers a read query to a COUNT(*) over the same relation and
// filter, ignoring projection, ordering, and the pagination window. It is the
// exact-count statement the backend runs alongside the windowed read to fill the
// total field of Content-Range (spec 10).
func CompileCount(d Dialect, q *ir.Query) (*Statement, *pgerr.APIError) {
	b := newBuilder(d)
	b.sb.WriteString("SELECT count(*) FROM ")
	b.sb.WriteString(b.qualify(q.Relation))
	if q.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*q.Where); err != nil {
			return nil, err
		}
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// CompileInsert lowers an insert (or upsert) to a parameterized INSERT. Every
// payload value is bound; absent columns take the engine DEFAULT or a bound NULL
// per the missing= preference. An upsert appends the dialect's ON CONFLICT
// clause. returning names the columns to read back (the projection for the
// representation, or the primary key for the Location header), or is empty when
// the client wants no rows back.
func CompileInsert(d Dialect, q *ir.Query, returning []string) (*Statement, *pgerr.APIError) {
	w := q.Write
	if w == nil || len(w.Rows) == 0 {
		return nil, pgerr.ErrParse("insert payload is empty")
	}
	b := newBuilder(d)
	b.sb.WriteString("INSERT INTO ")
	b.sb.WriteString(b.qualify(q.Relation))

	if len(w.Columns) == 0 {
		// An empty object inserts a row of engine defaults.
		b.sb.WriteString(" DEFAULT VALUES")
	} else {
		b.sb.WriteString(" (")
		for i, c := range w.Columns {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			b.sb.WriteString(d.QuoteIdent(c))
		}
		b.sb.WriteString(") VALUES ")
		for ri, row := range w.Rows {
			if ri > 0 {
				b.sb.WriteString(", ")
			}
			b.sb.WriteString("(")
			for ci, c := range w.Columns {
				if ci > 0 {
					b.sb.WriteString(", ")
				}
				if val, ok := row[c]; ok {
					b.sb.WriteString(b.bind(writeArg(val)))
				} else if w.Missing == ir.MissingNull {
					b.sb.WriteString(b.bind(nil))
				} else {
					b.sb.WriteString("DEFAULT")
				}
			}
			b.sb.WriteString(")")
		}
	}

	if w.Conflict != nil {
		if err := b.writeConflict(w); err != nil {
			return nil, err
		}
	}
	if err := b.writeReturning(returning); err != nil {
		return nil, err
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// CompileUpdate lowers a patch to a parameterized UPDATE ... SET ... WHERE. The
// SET columns are written in a deterministic order; the filter tree becomes the
// WHERE so a patch without a filter touches every row (matching PostgREST).
func CompileUpdate(d Dialect, q *ir.Query, returning []string) (*Statement, *pgerr.APIError) {
	w := q.Write
	if w == nil || len(w.Set) == 0 {
		return nil, pgerr.ErrParse("update payload is empty")
	}
	b := newBuilder(d)
	b.sb.WriteString("UPDATE ")
	b.sb.WriteString(b.qualify(q.Relation))
	b.sb.WriteString(" SET ")
	cols := sortedKeys(w.Set)
	for i, c := range cols {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		b.sb.WriteString(d.QuoteIdent(c))
		b.sb.WriteString(" = ")
		b.sb.WriteString(b.bind(writeArg(w.Set[c])))
	}
	if q.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*q.Where); err != nil {
			return nil, err
		}
	}
	if err := b.writeReturning(returning); err != nil {
		return nil, err
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// CompileDelete lowers a delete to a parameterized DELETE ... WHERE. As with
// update, a delete without a filter removes every row.
func CompileDelete(d Dialect, q *ir.Query, returning []string) (*Statement, *pgerr.APIError) {
	b := newBuilder(d)
	b.sb.WriteString("DELETE FROM ")
	b.sb.WriteString(b.qualify(q.Relation))
	if q.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*q.Where); err != nil {
			return nil, err
		}
	}
	if err := b.writeReturning(returning); err != nil {
		return nil, err
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// writeConflict appends the upsert clause built from the write's conflict spec,
// asking the dialect for the engine's spelling.
func (b *builder) writeConflict(w *ir.WriteSpec) *pgerr.APIError {
	spec := UpsertSpec{Ignore: w.Conflict.Resolution == ir.ConflictIgnore}
	for _, t := range w.Conflict.Target {
		spec.Target = append(spec.Target, b.d.QuoteIdent(t))
	}
	if !spec.Ignore {
		for _, c := range w.Columns {
			spec.Update = append(spec.Update, b.d.QuoteIdent(c))
		}
	}
	clause, err := b.d.Upsert(spec)
	if err != nil {
		return pgerr.ErrInternal(err.Error())
	}
	b.sb.WriteString(" ")
	b.sb.WriteString(clause)
	return nil
}

// writeReturning appends the dialect's row-returning clause for the given
// columns, or nothing when none are requested. An engine that cannot return
// written rows reports it here as a clear unsupported error.
func (b *builder) writeReturning(cols []string) *pgerr.APIError {
	if len(cols) == 0 {
		return nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = b.d.QuoteIdent(c)
	}
	clause, ok := b.d.Returning(quoted)
	if !ok {
		return pgerr.ErrUnsupported("returning written rows", "sql")
	}
	b.sb.WriteString(" ")
	b.sb.WriteString(clause)
	return nil
}

// writeArg converts a decoded JSON payload value to a driver argument. Numbers
// arrive as json.Number (the decoder preserves integer precision); objects and
// arrays are re-encoded to their JSON text so they land in a json/text column.
func writeArg(v ir.Value) any {
	switch x := v.JSON.(type) {
	case nil:
		return nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case map[string]any, []any:
		bs, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		return string(bs)
	default:
		return x
	}
}

// sortedKeys returns the keys of a value map in lexical order, for deterministic
// SQL.
func sortedKeys(m map[string]ir.Value) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// qualify renders a possibly schema-qualified relation reference, each part
// quoted by the dialect.
func (b *builder) qualify(r ir.Ref) string {
	if r.Schema == "" {
		return b.d.QuoteIdent(r.Name)
	}
	return b.d.QuoteIdent(r.Schema) + "." + b.d.QuoteIdent(r.Name)
}

// writeSelect emits the projection list. An empty select means all columns.
func (b *builder) writeSelect(items []ir.SelectItem) *pgerr.APIError {
	if len(items) == 0 {
		b.sb.WriteString("*")
		return nil
	}
	for i, it := range items {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		col, ok := it.(ir.Column)
		if !ok {
			return pgerr.ErrUnsupported("aggregates and embedded resources in select", "sql")
		}
		expr, err := b.columnExpr(col)
		if err != nil {
			return err
		}
		b.sb.WriteString(expr)
		// Alias the output so the renderer sees the PostgREST key, not the raw
		// column. Only needed when the key differs from the bare column name.
		if name := col.Name(); name != "" && name != lastPath(col.Path) {
			b.sb.WriteString(" AS ")
			b.sb.WriteString(b.d.QuoteIdent(name))
		}
	}
	return nil
}

// columnExpr renders a base column with an optional cast. JSON sub-paths are a
// later subsystem; a column carrying one is rejected explicitly.
func (b *builder) columnExpr(c ir.Column) (string, *pgerr.APIError) {
	if len(c.Path) == 1 && c.Path[0] == "*" && c.Last == ir.JSONNone && c.Cast == "" {
		if b.qual == "" {
			return "*", nil
		}
		return b.qual + ".*", nil
	}
	if len(c.Path) != 1 || c.Last != ir.JSONNone {
		return "", pgerr.ErrUnsupported("JSON path projection", "sql")
	}
	expr := b.colRef(c.Path[0])
	if c.Cast != "" {
		expr = b.d.Cast(expr, c.Cast)
	}
	return expr, nil
}

func lastPath(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}

// writeCond lowers a filter-tree node.
func (b *builder) writeCond(c ir.Cond) *pgerr.APIError {
	switch n := c.(type) {
	case ir.And:
		return b.writeLogical(n.Kids, " AND ")
	case ir.Or:
		return b.writeLogical(n.Kids, " OR ")
	case ir.Not:
		b.sb.WriteString("NOT (")
		if err := b.writeCond(n.Kid); err != nil {
			return err
		}
		b.sb.WriteString(")")
		return nil
	case ir.Compare:
		return b.writeCompare(n)
	default:
		return pgerr.ErrInternal(fmt.Sprintf("unknown filter node %T", c))
	}
}

func (b *builder) writeLogical(kids []ir.Cond, sep string) *pgerr.APIError {
	if len(kids) == 0 {
		return nil
	}
	b.sb.WriteString("(")
	for i, k := range kids {
		if i > 0 {
			b.sb.WriteString(sep)
		}
		if err := b.writeCond(k); err != nil {
			return err
		}
	}
	b.sb.WriteString(")")
	return nil
}

// writeCompare lowers a single column-operator-value predicate. The column is a
// base column for now (JSON-path filters arrive with the JSON subsystem).
func (b *builder) writeCompare(c ir.Compare) *pgerr.APIError {
	if len(c.Path) != 1 {
		return pgerr.ErrUnsupported("JSON path filters", "sql")
	}
	col := b.colRef(c.Path[0])

	var frag string
	var err *pgerr.APIError
	switch c.Op {
	case ir.OpEq, ir.OpNeq, ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte, ir.OpLike, ir.OpILike:
		frag = col + " " + binaryOp(c.Op) + " " + b.bind(c.Value.Text)
	case ir.OpIn:
		frag, err = b.writeIn(col, c.Value.List)
	case ir.OpIs:
		frag, err = b.writeIs(col, c.Value.Text)
	case ir.OpMatch, ir.OpIMatch:
		expr, ok := b.d.Regex(col, c.Value.Text, c.Op == ir.OpIMatch)
		if !ok {
			return pgerr.ErrUnsupported("regular-expression match", "sql")
		}
		// Regex returns an already-formed boolean expression carrying PatternMark
		// where the bound pattern placeholder goes.
		frag = strings.Replace(expr, PatternMark, b.bind(c.Value.Text), 1)
	default:
		return pgerr.ErrUnsupported("filter operator "+opName(c.Op), "sql")
	}
	if err != nil {
		return err
	}
	if c.Negate {
		frag = "NOT (" + frag + ")"
	}
	b.sb.WriteString(frag)
	return nil
}

func (b *builder) writeIn(col string, list []string) (string, *pgerr.APIError) {
	if len(list) == 0 {
		// `col IN ()` is a syntax error; an empty IN matches nothing.
		return "1 = 0", nil
	}
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = b.bind(v)
	}
	return col + " IN (" + strings.Join(parts, ", ") + ")", nil
}

func (b *builder) writeIs(col, text string) (string, *pgerr.APIError) {
	switch text {
	case "null":
		return col + " IS NULL", nil
	case "not_null":
		return col + " IS NOT NULL", nil
	case "true":
		return col + " IS " + b.d.BoolValue(true), nil
	case "false":
		return col + " IS " + b.d.BoolValue(false), nil
	default:
		return "", pgerr.ErrParse("unknown is value " + text)
	}
}

// writeOrder emits the ORDER BY, delegating NULLs placement to the dialect.
func (b *builder) writeOrder(terms []ir.OrderTerm) *pgerr.APIError {
	var sortKeys, orderTerms []string
	for _, t := range terms {
		if len(t.Path) != 1 {
			return pgerr.ErrUnsupported("JSON path ordering", "sql")
		}
		col := b.colRef(t.Path[0])
		dir := "ASC"
		if t.Desc {
			dir = "DESC"
		}
		sortKey, orderTerm := b.d.NullsOrder(col, dir, t.Desc, t.NullsFirst)
		if sortKey != "" {
			sortKeys = append(sortKeys, sortKey)
		}
		orderTerms = append(orderTerms, orderTerm)
	}
	b.sb.WriteString(" ORDER BY ")
	all := make([]string, 0, len(sortKeys)+len(orderTerms))
	all = append(all, sortKeys...)
	all = append(all, orderTerms...)
	b.sb.WriteString(strings.Join(all, ", "))
	return nil
}

// binaryOp maps an infix operator to its SQL spelling. Only the operators with a
// direct infix form go through here; the rest are handled in writeCompare.
func binaryOp(op ir.Op) string {
	switch op {
	case ir.OpEq:
		return "="
	case ir.OpNeq:
		return "<>"
	case ir.OpGt:
		return ">"
	case ir.OpGte:
		return ">="
	case ir.OpLt:
		return "<"
	case ir.OpLte:
		return "<="
	case ir.OpLike:
		return "LIKE"
	case ir.OpILike:
		return "LIKE" // case-insensitivity handled by dialect collation in later work
	default:
		return "="
	}
}

func opName(op ir.Op) string {
	names := map[ir.Op]string{
		ir.OpIsDistinct: "isdistinct", ir.OpFTS: "fts", ir.OpContains: "cs",
		ir.OpContained: "cd", ir.OpOverlap: "ov", ir.OpRangeSL: "sl",
		ir.OpRangeSR: "sr", ir.OpRangeNXR: "nxr", ir.OpRangeNXL: "nxl",
		ir.OpRangeAdj: "adj",
	}
	if n, ok := names[op]; ok {
		return n
	}
	return fmt.Sprintf("op(%d)", int(op))
}
