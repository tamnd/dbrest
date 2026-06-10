package sqlgen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
)

// CountColName is the synthetic column appended by CompileReadCounted to carry
// the window count alongside the result rows. Backends strip it from cols/rows
// after extracting the total, keeping the representation clean.
const CountColName = "_pgrst_count"

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
	return compileReadPlain(d, q, false)
}

// CompileReadCounted lowers a non-embedded read query to a SELECT that appends
// count(*) OVER () AS "_pgrst_count" to the projection. This lets the backend
// retrieve both the rows and the total row-count in a single query, avoiding
// a separate COUNT statement. It is only valid for non-embedded queries; an
// embedded query returns an internal error.
func CompileReadCounted(d Dialect, q *ir.Query) (*Statement, *pgerr.APIError) {
	if len(q.Embeds) > 0 {
		return nil, pgerr.ErrInternal("CompileReadCounted called on embedded query")
	}
	return compileReadPlain(d, q, true)
}

// compileReadPlain compiles a non-embedded read query. When withCount is true,
// count(*) OVER () AS "_pgrst_count" is appended to the SELECT list so callers
// can extract the total alongside the result rows.
func compileReadPlain(d Dialect, q *ir.Query, withCount bool) (*Statement, *pgerr.APIError) {
	b := newBuilder(d)
	b.sb.WriteString("SELECT ")

	if err := b.writeSelect(q.Select); err != nil {
		return nil, err
	}

	if withCount {
		b.sb.WriteString(`, count(*) OVER () AS "`)
		b.sb.WriteString(CountColName)
		b.sb.WriteString(`"`)
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
	// Update carries the payload columns for both resolutions. A merge sets each
	// to its excluded value; an ignore on an engine whose ignore form is a no-op
	// update (MySQL's ON DUPLICATE KEY UPDATE col = col) needs the same columns.
	// Engines that spell ignore as a distinct clause (PostgreSQL/SQLite DO
	// NOTHING) read Ignore first and never look at Update, so passing it is inert.
	for _, c := range w.Columns {
		spec.Update = append(spec.Update, b.d.QuoteIdent(c))
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

// WriteArg converts a decoded JSON payload value to a driver argument. Numbers
// arrive as json.Number (the decoder preserves integer precision); objects and
// arrays are re-encoded to their JSON text so they land in a json/text column.
// It is exported for backends (e.g. the COPY path) that need the same coercion
// without going through the SQL builder.
func WriteArg(v ir.Value) any { return writeArg(v) }

// writeArg is the unexported implementation used by the builder methods.
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
	case []any:
		// PostgreSQL array columns use {elem1,elem2} input syntax, not JSON
		// ["elem1","elem2"]. Build the array literal so the server-side cast
		// from text to text[]/int4[]/etc. succeeds with or without type OIDs.
		return pgArrayLiteral(x)
	case map[string]any:
		bs, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		return string(bs)
	default:
		return x
	}
}

// pgArrayLiteral converts a JSON array into a PostgreSQL array literal string
// of the form {elem1,"elem with spaces",NULL}. Elements that are plain
// alphanumeric strings (and json.Number/bool) are emitted unquoted; strings
// that contain commas, braces, backslashes, double-quotes, or whitespace are
// double-quoted with internal backslash escaping, matching PostgreSQL's own
// array output format.
func pgArrayLiteral(elems []any) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, e := range elems {
		if i > 0 {
			sb.WriteByte(',')
		}
		switch v := e.(type) {
		case nil:
			sb.WriteString("NULL")
		case bool:
			if v {
				sb.WriteByte('t')
			} else {
				sb.WriteByte('f')
			}
		case json.Number:
			sb.WriteString(v.String())
		case float64:
			sb.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		case int64:
			sb.WriteString(strconv.FormatInt(v, 10))
		case string:
			if pgArrayElemNeedsQuote(v) {
				sb.WriteByte('"')
				for _, c := range v {
					if c == '"' || c == '\\' {
						sb.WriteByte('\\')
					}
					sb.WriteRune(c)
				}
				sb.WriteByte('"')
			} else {
				sb.WriteString(v)
			}
		default:
			// Nested arrays or unexpected types: fall back to JSON.
			if bs, err := json.Marshal(v); err == nil {
				sb.Write(bs)
			}
		}
	}
	sb.WriteByte('}')
	return sb.String()
}

// pgArrayElemNeedsQuote reports whether a string element must be double-quoted
// in a PostgreSQL array literal. Quoting is required for strings that contain
// commas, braces, backslashes, double-quotes, or whitespace, or that could be
// mistaken for NULL or a bare number.
func pgArrayElemNeedsQuote(s string) bool {
	if s == "" || strings.EqualFold(s, "null") {
		return true
	}
	for _, c := range s {
		if c == ',' || c == '{' || c == '}' || c == '"' || c == '\\' ||
			c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return true
		}
	}
	return false
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
	case ir.OpEq, ir.OpNeq:
		// Boolean literals "true"/"false" are rendered via BoolValue so engines
		// without a native BOOL type (MySQL TINYINT) produce correct predicates
		// (e.g. done = 1 rather than done = 'true' which MySQL coerces to 0).
		switch c.Value.Text {
		case "true":
			frag = col + " " + binaryOp(c.Op) + " " + b.d.BoolValue(true)
		case "false":
			frag = col + " " + binaryOp(c.Op) + " " + b.d.BoolValue(false)
		default:
			frag = col + " " + binaryOp(c.Op) + " " + b.bind(c.Value.Text)
		}
	case ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte, ir.OpLike:
		if c.Quant != ir.QNone {
			frag, err = b.writeLikeQuantified(col, ir.OpLike, c.Quant, c.Value.List)
		} else {
			frag = col + " " + binaryOp(c.Op) + " " + b.bind(c.Value.Text)
		}
	case ir.OpILike:
		if c.Quant != ir.QNone {
			frag, err = b.writeLikeQuantified(col, ir.OpILike, c.Quant, c.Value.List)
		} else {
			var ok bool
			frag, ok = b.d.ILike(col, b.bind(c.Value.Text))
			if !ok {
				return pgerr.ErrUnsupported("case-insensitive LIKE", "sql")
			}
		}
	case ir.OpIn:
		frag, err = b.writeIn(col, c.Value.List)
	case ir.OpIs:
		frag, err = b.writeIs(col, c.Value.Text)
	case ir.OpMatch, ir.OpIMatch:
		// A pattern that uses a construct the engine's regex flavor lacks (a
		// backreference on RE2-backed SQLite) is rejected before lowering, naming
		// the feature, rather than matching a quietly different set. See spec 21.
		if feat := b.d.RegexFeatureGap(c.Value.Text); feat != "" {
			return pgerr.ErrUnsupported(feat, "sql")
		}
		expr, ok := b.d.Regex(col, c.Value.Text, c.Op == ir.OpIMatch)
		if !ok {
			return pgerr.ErrUnsupported("regular-expression match", "sql")
		}
		// Regex returns an already-formed boolean expression carrying PatternMark
		// where the bound pattern placeholder goes.
		frag = strings.Replace(expr, PatternMark, b.bind(c.Value.Text), 1)
	case ir.OpFTS:
		frag, err = b.writeFTS(c, col)
	case ir.OpIsDistinct:
		frag = col + " IS DISTINCT FROM " + b.bind(c.Value.Text)
	case ir.OpContains, ir.OpContained, ir.OpOverlap:
		var sqlOp string
		switch c.Op {
		case ir.OpContains:
			sqlOp = "@>"
		case ir.OpContained:
			sqlOp = "<@"
		default:
			sqlOp = "&&"
		}
		// Normalize the PostgreSQL {a,b} array literal to the engine's format
		// before binding; the dialect is a no-op for engines that accept {a,b}.
		val := b.bind(b.d.ArrayLiteral(c.Value.Text))
		var ok bool
		frag, ok = b.d.ArrayOp(col, sqlOp, val)
		if !ok {
			return pgerr.ErrUnsupported("array operator "+sqlOp, "sql")
		}
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

// writeFTS lowers a full-text predicate by handing the resolved covering index
// (when the planner found one) to the dialect. A dialect that needs an index and
// got none reports ok=false, which becomes the PGRST127 naming the column, so a
// missing full-text structure is a clear error rather than a silent scan. col is
// the already-qualified column reference. See spec 21.
func (b *builder) writeFTS(c ir.Compare, col string) (string, *pgerr.APIError) {
	var ref *FullTextRef
	if c.FullText != nil {
		rowid := c.FullText.RowidColumn
		if rowid == "" {
			rowid = "rowid"
		}
		ref = &FullTextRef{
			Table:    b.d.QuoteIdent(c.FullText.Name),
			RowidRef: b.colRef(rowid),
		}
	}
	expr, bindVal, ok := b.d.FullText(col, ref, c.FTS, c.Config, c.Value.Text)
	if !ok {
		return "", pgerr.ErrFullTextUnavailable(c.Path[0], "sql")
	}
	return strings.Replace(expr, PatternMark, b.bind(bindVal), 1), nil
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

// writeLikeQuantified expands like(any)/{...} and like(all)/{...} into a
// conjunction or disjunction of individual LIKE / ILIKE predicates. An empty
// list generates a no-match literal (1 = 0) for ANY and always-match (1 = 1)
// for ALL, consistent with SQL ANY/ALL semantics over an empty set.
func (b *builder) writeLikeQuantified(col string, op ir.Op, q ir.Quant, list []string) (string, *pgerr.APIError) {
	if len(list) == 0 {
		if q == ir.QAny {
			return "1 = 0", nil
		}
		return "1 = 1", nil
	}
	sep := " OR "
	if q == ir.QAll {
		sep = " AND "
	}
	parts := make([]string, len(list))
	for i, pat := range list {
		bound := b.bind(pat)
		if op == ir.OpILike {
			expr, ok := b.d.ILike(col, bound)
			if !ok {
				return "", pgerr.ErrUnsupported("case-insensitive LIKE", "sql")
			}
			parts[i] = expr
		} else {
			parts[i] = col + " LIKE " + bound
		}
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, sep) + ")", nil
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
		return "ILIKE"
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
