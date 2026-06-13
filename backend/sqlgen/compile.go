package sqlgen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/pgtypes"
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
	// parentRef is how an EmbedPredicate's EXISTS/NOT EXISTS correlates back to the
	// outer row: the parent alias (t0) in an embedded read, or the qualified table
	// name in a count where the parent has no alias. embeds is the parent query's
	// embed list an EmbedPredicate indexes into.
	parentRef string
	embeds    []ir.Embed
	// groupBy collects the non-aggregate projected column expressions while the
	// select list is written; when the projection also carries an aggregate, these
	// become the GROUP BY so the aggregate folds per distinct value. hasAgg records
	// whether any aggregate was seen.
	groupBy []string
	hasAgg  bool
	// ctxArgs are the reserved :request_* values an RPC body may bind when a
	// placeholder is not a declared parameter; see ContextArgs.
	ctxArgs map[string]any
	// computed maps the current relation's computed-field names to the schema of
	// the function backing each, so colRef can render a computed field as a function
	// call on the row instead of a bare column. It is swapped alongside qual when
	// descending into an embed, since each relation has its own computed fields.
	// rootRow is the row reference passed to a computed-field function when no alias
	// is in force (qual is empty): the unqualified base-relation name, which names
	// the row of the single FROM relation. See spec 11 (computed fields).
	computed map[string]string
	rootRow  string
	// reps maps the current relation's column names to their data-representation
	// cast functions (spec 11), swapped alongside computed when descending into an
	// embed. A column with one reformats on the wire: ToJSON on read, FromJSON on a
	// write value, FromText on a filter literal.
	reps map[string]ir.Rep
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
	if sch, ok := b.computed[name]; ok {
		// A computed field renders as schema.func(row): the row is the current alias
		// inside an embed, or the bare relation name at the top level (qual empty).
		rowArg := b.qual
		if rowArg == "" {
			rowArg = b.rootRow
		}
		return b.d.QuoteIdent(sch) + "." + b.d.QuoteIdent(name) + "(" + rowArg + ")"
	}
	if b.qual == "" {
		return b.d.QuoteIdent(name)
	}
	return b.qual + "." + b.d.QuoteIdent(name)
}

// useRelation points the builder's computed-field and data-representation
// rendering at one relation: the name-to-schema map for its computed fields, the
// column-to-cast map for its representations, and the unqualified name to pass as
// the row argument when no alias is in force. It returns the previous trio so a
// caller descending into an embed can restore the parent's on the way out.
func (b *builder) useRelation(q *ir.Query, relName string) (map[string]string, map[string]ir.Rep, string) {
	savedC, savedReps, savedR := b.computed, b.reps, b.rootRow
	b.computed = q.Computed
	b.reps = q.Reps
	b.rootRow = b.d.QuoteIdent(relName)
	return savedC, savedReps, savedR
}

// repCall renders a representation cast-function call: schema.func(arg). It is how
// a domain's to-json/from-text/from-json cast is applied, since PostgreSQL ignores
// the cast in the `::` operator and only the function does the reformatting.
func (b *builder) repCall(funcSchema, funcName, arg string) string {
	return b.d.QuoteIdent(funcSchema) + "." + b.d.QuoteIdent(funcName) + "(" + arg + ")"
}

// fromTextValue binds a filter operand, parsing it through the column's from-text
// data representation when one is present (spec 11). It mirrors PostgREST's
// pgFmtUnknownLiteralForField: the domain's text cast wraps the operand for every
// operator that compares against a typed value (eq, neq, the orderings, regex
// match, the array/range operators, and each IN element). The placeholder is
// typed text so the schema-qualified cast function resolves as a call rather than
// as the domain's own input syntax. PostgREST skips the parse for like/ilike (the
// operand is a wildcard pattern), full-text search, and is, so those callers bind
// the literal directly instead of going through here. A JSON-path operand is
// never a represented column, and a column with no from-text cast binds the
// literal unchanged.
func (b *builder) fromTextValue(colName string, isJSON bool, raw string) string {
	ph := b.bind(raw)
	if isJSON {
		return ph
	}
	if rep, ok := b.reps[colName]; ok && rep.FromTextFunc != "" {
		return b.repCall(rep.FromTextSchema, rep.FromTextFunc, ph+"::text")
	}
	return ph
}

// filterValue binds a comparison literal through the column's from-text data
// representation, the common path for eq/neq and the orderings.
func (b *builder) filterValue(c ir.Compare) string {
	return b.fromTextValue(c.Path[0], len(c.Path) > 1, c.Value.Text)
}

// writeValue binds an insert/update value, parsing it through the column's
// from-json data representation when one is present (spec 11): the body value is
// bound as json text and passed to the domain's json cast, the same parse
// PostgREST applies to a write. A column with no from-json cast binds the coerced
// value through writeArg as usual.
func (b *builder) writeValue(col string, v ir.Value, colType string) string {
	if rep, ok := b.reps[col]; ok && rep.FromJSONFunc != "" {
		js, err := json.Marshal(v.JSON)
		if err != nil {
			js = []byte("null")
		}
		return b.repCall(rep.FromJSONSchema, rep.FromJSONFunc, b.bind(string(js))+"::json")
	}
	return b.bind(writeArg(b.d, v, colType))
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
	b.useRelation(q, q.Relation.Name)
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

	// An aggregate folds over the rest of the projection: the plain columns become
	// the GROUP BY keys. With only aggregates and no plain column, the whole
	// relation is one group and no GROUP BY is emitted.
	if b.hasAgg && len(b.groupBy) > 0 {
		b.sb.WriteString(" GROUP BY ")
		b.sb.WriteString(strings.Join(b.groupBy, ", "))
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
	if err := b.writeCountFromAndPredicates(q); err != nil {
		return nil, err
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// CompileRowEstimateSource lowers a read query to a row-producing SELECT over the
// same relation and predicates the count covers, with no aggregate. A backend
// that estimates a count (count=planned / count=estimated) EXPLAINs this query
// and reads the planner's row estimate off the root node; the count(*) wrapper
// would instead estimate the aggregate's single output row. The empty target
// list (SELECT FROM) keeps it estimate-only: it is never fetched (item 07.7).
func CompileRowEstimateSource(d Dialect, q *ir.Query) (*Statement, *pgerr.APIError) {
	b := newBuilder(d)
	b.sb.WriteString("SELECT FROM ")
	if err := b.writeCountFromAndPredicates(q); err != nil {
		return nil, err
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// writeCountFromAndPredicates emits the parent relation and the predicates a
// count ranges over: the horizontal WHERE and an EXISTS per !inner embed, the
// same set the windowed read applies so an exact count matches its body. The
// caller has already written the SELECT list up to FROM.
func (b *builder) writeCountFromAndPredicates(q *ir.Query) *pgerr.APIError {
	b.useRelation(q, q.Relation.Name)
	parent := b.qualify(q.Relation)
	b.sb.WriteString(parent)

	// An embed-existence filter and an !inner embed's EXISTS both correlate to the
	// parent by its bare table name here, since a count gives the parent no alias.
	b.parentRef = parent
	b.embeds = q.Embeds

	wrote := false
	if q.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*q.Where); err != nil {
			return err
		}
		wrote = true
	}
	// The row query restricts the parent with an EXISTS per !inner embed
	// (compileReadEmbedded), so the count must carry the same predicates or
	// Content-Range disagrees with the body it accompanies.
	for i := range q.Embeds {
		emb := &q.Embeds[i]
		if emb.Join != ir.JoinInner {
			continue
		}
		if wrote {
			b.sb.WriteString(" AND ")
		} else {
			b.sb.WriteString(" WHERE ")
			wrote = true
		}
		if err := b.writeEmbedExists(emb, parent); err != nil {
			return err
		}
	}
	return nil
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
	b.useRelation(q, q.Relation.Name)
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
					b.sb.WriteString(b.writeValue(c, val, w.ColumnTypes[c]))
				} else if w.Missing == ir.MissingNull {
					b.sb.WriteString(b.bind(nil))
				} else {
					b.sb.WriteString("DEFAULT")
				}
			}
			b.sb.WriteString(")")
		}
	}

	// An upsert with no resolvable conflict target (a table or view without a
	// primary key, no on_conflict given) has nothing to merge or ignore on, so it
	// degrades to a plain INSERT, the same as PostgREST: a merge or ignore POST to
	// a key-less table inserts the rows and returns 201. Emitting ON CONFLICT here
	// would produce invalid SQL ("ON CONFLICT DO UPDATE requires inference").
	if w.Conflict != nil && len(w.Conflict.Target) > 0 {
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
	b.useRelation(q, q.Relation.Name)
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
		b.sb.WriteString(b.writeValue(c, w.Set[c], w.ColumnTypes[c]))
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
	b.useRelation(q, q.Relation.Name)
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
		// A data-representation column reads back through its to-json cast, the same
		// formatting a read applies, so return=representation carries what a later GET
		// would return (spec 11). The cast function would otherwise name the output
		// column after itself, so alias it back to the column name.
		if rep, ok := b.reps[c]; ok && rep.ToJSONFunc != "" {
			id := b.d.QuoteIdent(c)
			quoted[i] = b.repCall(rep.ToJSONSchema, rep.ToJSONFunc, id) + " AS " + id
			continue
		}
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
// arrive as json.Number (the decoder preserves integer precision); objects are
// re-encoded to their JSON text so they land in a json/text column; arrays go
// through the dialect, which knows whether the engine wants a PostgreSQL
// {a,b} array literal or JSON text. It is exported for backends (e.g. the
// MERGE path) that need the same coercion without going through the SQL
// builder.
func WriteArg(d Dialect, v ir.Value, colType string) any { return writeArg(d, v, colType) }

// writeArg is the unexported implementation used by the builder methods. colType
// is the target column's canonical type, which steers how a JSON array value is
// bound (see Dialect.ArrayArg); an empty colType keeps the engine default.
func writeArg(d Dialect, v ir.Value, colType string) any {
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
		return d.ArrayArg(x, colType)
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

// IsJSONArrayIndex reports whether a JSON path segment is an array index: a
// non-empty run of ASCII digits. PostgREST treats a digit hop (data->arr->0) as
// an array subscript rather than an object key, and the dialects spell it as
// one. A leading-zero or oversized run still counts as an index; the engine
// decides what a missing element yields.
func IsJSONArrayIndex(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return false
		}
	}
	return true
}

// JSONArrayArg re-encodes a decoded JSON array to its JSON text. It is the
// ArrayArg implementation for engines without array columns, where a write
// payload array lands in a json/text column and must read back as JSON.
func JSONArrayArg(elems []any) any {
	bs, err := json.Marshal(elems)
	if err != nil {
		return nil
	}
	return string(bs)
}

// PGArrayLiteral converts a JSON array into a PostgreSQL array literal string
// of the form {elem1,"elem with spaces",NULL}. Elements that are plain
// alphanumeric strings (and json.Number/bool) are emitted unquoted; strings
// that contain commas, braces, backslashes, double-quotes, or whitespace are
// double-quoted with internal backslash escaping, matching PostgreSQL's own
// array output format. It is the PostgreSQL Dialect's ArrayArg: the literal
// text lets the server-side cast from text to text[]/int4[]/etc. succeed with
// or without type OIDs.
func PGArrayLiteral(elems []any) string {
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
		switch v := it.(type) {
		case ir.Column:
			expr, err := b.columnExpr(v)
			if err != nil {
				return err
			}
			b.sb.WriteString(expr)
			// Alias the output so the renderer sees the PostgREST key, not the raw
			// column expression. Always alias when a cast is present, an explicit
			// alias was set, the column is a JSON path (data->>x names its column
			// after the last hop, the way upstream does), or a data representation
			// wrapped the column in a cast function (which would otherwise name the
			// output column after the function, not the column).
			if name := v.Name(); name != "" && (name != lastPath(v.Path) || v.Cast != "" || len(v.Path) > 1 || b.repAppliedToJSON(v)) {
				b.sb.WriteString(" AS ")
				b.sb.WriteString(b.d.QuoteIdent(name))
			}
			// A plain column alongside an aggregate is a GROUP BY key.
			b.groupBy = append(b.groupBy, expr)
		case ir.Aggregate:
			expr, err := b.aggregateExpr(v)
			if err != nil {
				return err
			}
			b.sb.WriteString(expr)
			b.sb.WriteString(" AS ")
			b.sb.WriteString(b.d.QuoteIdent(v.Name()))
			b.hasAgg = true
		default:
			return pgerr.ErrUnsupported("embedded resources in select", "sql")
		}
	}
	return nil
}

// aggregateExpr renders an aggregate call: count(*) for a bare count, or
// func(arg) over the aggregated column, with an optional input cast on the
// column and an optional output cast wrapping the result.
func (b *builder) aggregateExpr(a ir.Aggregate) (string, *pgerr.APIError) {
	fn := a.Func.String()
	var inner string
	if a.Arg == nil {
		inner = fn + "(*)"
	} else {
		arg, err := b.columnExpr(*a.Arg)
		if err != nil {
			return "", err
		}
		inner = fn + "(" + arg + ")"
	}
	if a.Cast != "" {
		inner = b.d.Cast(inner, a.Cast)
	}
	return inner, nil
}

// jsonPathExpr lowers a base column carrying a JSON sub-path through the dialect.
// hops are the segments after the base; last reports the final ->/->> kind. An
// engine without JSON paths reports ok=false and the request is PGRST127.
func (b *builder) jsonPathExpr(base string, hops []string, last ir.JSONStep) (string, *pgerr.APIError) {
	frag, ok := b.d.JSONPath(base, hops, last == ir.JSONArrow2)
	if !ok {
		return "", pgerr.ErrUnsupported("JSON path", "sql")
	}
	return frag, nil
}

// columnExpr renders a base column with an optional cast, lowering a JSON
// sub-path (col->a->>b) through the dialect when the column carries one.
func (b *builder) columnExpr(c ir.Column) (string, *pgerr.APIError) {
	if len(c.Path) == 1 && c.Path[0] == "*" && c.Last == ir.JSONNone && c.Cast == "" {
		if b.qual == "" {
			return "*", nil
		}
		return b.qual + ".*", nil
	}
	var expr string
	if len(c.Path) > 1 {
		frag, err := b.jsonPathExpr(b.colRef(c.Path[0]), c.Path[1:], c.Last)
		if err != nil {
			return "", err
		}
		expr = frag
	} else {
		expr = b.colRef(c.Path[0])
		// A data-representation column reformats on output through its to-json cast
		// (spec 11): the stored value is passed to the cast function, which yields the
		// json the response carries. An explicit client cast (col::type) opts out, the
		// client having asked for a specific rendering instead.
		if c.Cast == "" {
			if rep, ok := b.reps[c.Path[0]]; ok && rep.ToJSONFunc != "" {
				expr = b.repCall(rep.ToJSONSchema, rep.ToJSONFunc, expr)
			}
		}
	}
	if c.Cast != "" {
		expr = b.d.Cast(expr, c.Cast)
	}
	return expr, nil
}

// repAppliedToJSON reports whether a plain base column carries a to-json data
// representation that columnExpr will apply, so writeSelect knows to alias the
// projection to the column name (the cast function would otherwise name the output
// column after itself). A JSON sub-path or an explicit client cast opts out, the
// same conditions columnExpr checks.
func (b *builder) repAppliedToJSON(c ir.Column) bool {
	if len(c.Path) != 1 || c.Cast != "" {
		return false
	}
	rep, ok := b.reps[c.Path[0]]
	return ok && rep.ToJSONFunc != ""
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
	case ir.EmbedPredicate:
		return b.writeEmbedPredicate(n)
	default:
		return pgerr.ErrInternal(fmt.Sprintf("unknown filter node %T", c))
	}
}

// writeEmbedPredicate lowers an embed-existence filter (films?actors=is.null /
// not.is.null). not.is.null is a semi-join, the same EXISTS an !inner embed
// adds; is.null is its anti-join complement (NOT EXISTS). The correlation hangs
// off parentRef so the predicate works both in an embedded read (alias t0) and
// in a plain count (the bare table name). See item 01.12.
func (b *builder) writeEmbedPredicate(p ir.EmbedPredicate) *pgerr.APIError {
	if p.Index < 0 || p.Index >= len(b.embeds) {
		return pgerr.ErrInternal("embed predicate index out of range")
	}
	if !p.Exists {
		b.sb.WriteString("NOT ")
	}
	return b.writeEmbedExists(&b.embeds[p.Index], b.parentRef)
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

// writeCompare lowers a single column-operator-value predicate. A column may
// carry a JSON sub-path (data->>field), lowered through the dialect.
func (b *builder) writeCompare(c ir.Compare) *pgerr.APIError {
	col := b.colRef(c.Path[0])
	isJSON := len(c.Path) > 1
	if isJSON {
		frag, err := b.jsonPathExpr(col, c.Path[1:], c.Last)
		if err != nil {
			return err
		}
		col = frag
	}

	// A quantified filter (op(any)/op(all) over a {…} list) expands to a disjunction
	// or conjunction of the real operator, one predicate per element (item 01.1).
	if c.Quant != ir.QNone {
		frag, err := b.writeQuantified(col, c)
		if err != nil {
			return err
		}
		if c.Negate {
			frag = "NOT (" + frag + ")"
		}
		b.sb.WriteString(frag)
		return nil
	}

	var frag string
	var err *pgerr.APIError
	switch c.Op {
	case ir.OpEq, ir.OpNeq:
		// Boolean literals "true"/"false" are rendered via BoolValue so engines
		// without a native BOOL type (MySQL TINYINT) produce correct predicates
		// (e.g. done = 1 rather than done = 'true' which MySQL coerces to 0). The
		// coercion is column-type-aware: against a non-boolean column (a text
		// column literally holding the word "true") the words stay text, matching
		// PostgreSQL's type-driven coercion (item 07.4). An unknown column type
		// keeps the boolean rendering, the common ?col=is-not-the-point filter.
		// A JSON ->>/-> extract is a text/json value, never a typed boolean column,
		// so the words "true"/"false" bind as text and a JSON field holding the
		// string still matches (the eq.true coercion is column-type driven).
		boolColumn := !isJSON && (c.ColumnType == "" || pgtypes.ClassOf(c.ColumnType) == pgtypes.ClassBool)
		switch {
		case c.Value.Text == "true" && boolColumn:
			frag = col + " " + binaryOp(c.Op) + " " + b.d.BoolValue(true)
		case c.Value.Text == "false" && boolColumn:
			frag = col + " " + binaryOp(c.Op) + " " + b.d.BoolValue(false)
		default:
			frag = col + " " + binaryOp(c.Op) + " " + b.filterValue(c)
		}
	case ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte:
		frag = col + " " + binaryOp(c.Op) + " " + b.filterValue(c)
	case ir.OpLike:
		frag = col + " " + binaryOp(c.Op) + " " + b.bind(c.Value.Text)
	case ir.OpILike:
		var ok bool
		frag, ok = b.d.ILike(col, b.bind(c.Value.Text))
		if !ok {
			return pgerr.ErrUnsupported("case-insensitive LIKE", "sql")
		}
	case ir.OpIn:
		frag, err = b.writeIn(col, c.Path[0], isJSON, c.Value.List)
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
		// where the bound pattern placeholder goes. A represented column parses the
		// pattern through its from-text cast, as PostgREST does for match/imatch.
		frag = strings.Replace(expr, PatternMark, b.fromTextValue(c.Path[0], isJSON, c.Value.Text), 1)
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
		// before binding; the dialect is a no-op for engines that accept {a,b}. A
		// represented column parses the literal through its from-text cast, matching
		// PostgREST's simple-operator path.
		val := b.fromTextValue(c.Path[0], isJSON, b.d.ArrayLiteral(c.Value.Text))
		var ok bool
		frag, ok = b.d.ArrayOp(col, sqlOp, val, c.ColumnType)
		if !ok {
			return pgerr.ErrUnsupported("array operator "+sqlOp, "sql")
		}
	case ir.OpRangeSL, ir.OpRangeSR, ir.OpRangeNXR, ir.OpRangeNXL, ir.OpRangeAdj:
		var rop string
		switch c.Op {
		case ir.OpRangeSL:
			rop = "<<"
		case ir.OpRangeSR:
			rop = ">>"
		case ir.OpRangeNXR:
			rop = "&<"
		case ir.OpRangeNXL:
			rop = "&>"
		default:
			rop = "-|-"
		}
		var ok bool
		frag, ok = b.d.RangeOp(col, rop, b.fromTextValue(c.Path[0], isJSON, c.Value.Text))
		if !ok {
			return pgerr.ErrUnsupported("range operator "+opName(c.Op), "sql")
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
	expr, bindVal, ok := b.d.FullText(col, c.ColumnType, ref, c.FTS, c.Config, c.Value.Text)
	if !ok {
		return "", pgerr.ErrFullTextUnavailable(c.Path[0], "sql")
	}
	return strings.Replace(expr, PatternMark, b.bind(bindVal), 1), nil
}

func (b *builder) writeIn(col, colName string, isJSON bool, list []string) (string, *pgerr.APIError) {
	if len(list) == 0 {
		// `col IN ()` is a syntax error; an empty IN matches nothing.
		return "1 = 0", nil
	}
	rep, hasRep := b.reps[colName]
	useRep := !isJSON && hasRep && rep.FromTextFunc != ""
	// On an engine that binds the list as a single array (PostgreSQL's = ANY), every
	// list length is one prepared statement instead of one per length. The element
	// quoting is PostgreSQL's array-literal format, the same the array operators use,
	// so a value with a comma or brace stays a single element. The bind happens only
	// on this branch so the expansion path's placeholder numbering is unaffected.
	if frag, ok := b.d.InList(col); ok {
		elems := make([]any, len(list))
		for i, v := range list {
			elems[i] = v
		}
		ph := b.bind(PGArrayLiteral(elems))
		operand := ph
		if useRep {
			// A represented column parses each element through its from-text cast,
			// applied over the unpacked array, matching PostgREST's
			// pgFmtArrayLiteralForField.
			operand = "(SELECT " + b.repCall(rep.FromTextSchema, rep.FromTextFunc, "unnest("+ph+"::text[])") + ")"
		}
		return strings.Replace(frag, PatternMark, operand, 1), nil
	}
	parts := make([]string, len(list))
	for i, v := range list {
		if useRep {
			parts[i] = b.repCall(rep.FromTextSchema, rep.FromTextFunc, b.bind(v)+"::text")
		} else {
			parts[i] = b.bind(v)
		}
	}
	return col + " IN (" + strings.Join(parts, ", ") + ")", nil
}

// writeQuantified expands a quantified filter (op(any)/op(all) over a {…} list)
// into a disjunction (ANY) or conjunction (ALL) of the real operator, one
// predicate per element. An empty list is a no-match literal (1 = 0) for ANY and
// always-match (1 = 1) for ALL, consistent with SQL ANY/ALL over an empty set,
// though the parser now rejects an empty list upstream. See item 01.1.
func (b *builder) writeQuantified(col string, c ir.Compare) (string, *pgerr.APIError) {
	list := c.Value.List
	if len(list) == 0 {
		if c.Quant == ir.QAny {
			return "1 = 0", nil
		}
		return "1 = 1", nil
	}
	sep := " OR "
	if c.Quant == ir.QAll {
		sep = " AND "
	}
	colName := c.Path[0]
	isJSON := len(c.Path) > 1
	parts := make([]string, len(list))
	for i, v := range list {
		frag, err := b.quantElem(col, colName, isJSON, c.Op, v)
		if err != nil {
			return "", err
		}
		parts[i] = frag
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, sep) + ")", nil
}

// quantElem lowers one element of a quantified list to its single-operator SQL
// predicate, using the operator's real infix/regex/ILIKE form.
func (b *builder) quantElem(col, colName string, isJSON bool, op ir.Op, v string) (string, *pgerr.APIError) {
	switch op {
	case ir.OpEq, ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte:
		return col + " " + binaryOp(op) + " " + b.fromTextValue(colName, isJSON, v), nil
	case ir.OpLike:
		// like carries a wildcard pattern, so PostgREST binds it raw even on a
		// represented column.
		return col + " " + binaryOp(op) + " " + b.bind(v), nil
	case ir.OpILike:
		expr, ok := b.d.ILike(col, b.bind(v))
		if !ok {
			return "", pgerr.ErrUnsupported("case-insensitive LIKE", "sql")
		}
		return expr, nil
	case ir.OpMatch, ir.OpIMatch:
		if feat := b.d.RegexFeatureGap(v); feat != "" {
			return "", pgerr.ErrUnsupported(feat, "sql")
		}
		expr, ok := b.d.Regex(col, v, op == ir.OpIMatch)
		if !ok {
			return "", pgerr.ErrUnsupported("regular-expression match", "sql")
		}
		return strings.Replace(expr, PatternMark, b.fromTextValue(colName, isJSON, v), 1), nil
	default:
		return "", pgerr.ErrUnsupported("quantifier on "+opName(op), "sql")
	}
}

func (b *builder) writeIs(col, text string) (string, *pgerr.APIError) {
	switch text {
	case "null":
		return col + " IS NULL", nil
	case "not_null":
		return col + " IS NOT NULL", nil
	case "true":
		if frag, ok := b.d.IsBool(col, true); ok {
			return frag, nil
		}
		return col + " IS " + b.d.BoolValue(true), nil
	case "false":
		if frag, ok := b.d.IsBool(col, false); ok {
			return frag, nil
		}
		return col + " IS " + b.d.BoolValue(false), nil
	case "unknown":
		if frag, ok := b.d.IsUnknown(col); ok {
			return frag, nil
		}
		return col + " IS NULL", nil
	default:
		return "", pgerr.ErrParse("unknown is value " + text)
	}
}

// writeOrder emits the ORDER BY, delegating NULLs placement to the dialect.
func (b *builder) writeOrder(terms []ir.OrderTerm) *pgerr.APIError {
	// The parent reference for a related-order subquery is the qualifier in force
	// as the ORDER BY is written (t0 in an embedded read, the bare table name in a
	// count).
	parentAlias := b.qual
	var sortKeys, orderTerms []string
	for _, t := range terms {
		var col string
		if t.Rel != "" {
			frag, err := b.relatedOrderExpr(t, parentAlias)
			if err != nil {
				return err
			}
			col = frag
		} else {
			col = b.colRef(t.Path[0])
			if len(t.Path) > 1 {
				frag, err := b.jsonPathExpr(col, t.Path[1:], t.Last)
				if err != nil {
					return err
				}
				col = frag
			}
		}
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

// relatedOrderExpr lowers an order=rel(col) term to a correlated scalar subquery
// selecting the embed's column for the matching to-one row: a parent with no
// related row yields NULL, which the dialect's NULLs placement then orders. The
// embed is matched by the same written name the planner validated, and its
// to-one join condition correlates the subquery back to the parent (item 07.6).
func (b *builder) relatedOrderExpr(t ir.OrderTerm, parentAlias string) (string, *pgerr.APIError) {
	emb := b.findEmbed(t.Rel)
	if emb == nil {
		// The planner validates the relation is embedded; reaching here is a bug.
		return "", pgerr.ErrInternal("related order names an unresolved embed: " + t.Rel)
	}
	rel := emb.Rel
	b.aliasN++
	alias := "o" + strconv.Itoa(b.aliasN)

	saved := b.qual
	b.qual = alias
	savedC, savedReps, savedR := b.useRelation(&emb.Query, rel.Target.Name)
	col := b.colRef(t.Path[0])
	if len(t.Path) > 1 {
		frag, err := b.jsonPathExpr(col, t.Path[1:], t.Last)
		if err != nil {
			b.qual = saved
			b.computed, b.reps, b.rootRow = savedC, savedReps, savedR
			return "", err
		}
		col = frag
	}
	b.qual = saved
	b.computed, b.reps, b.rootRow = savedC, savedReps, savedR

	from, cond := b.embedSource(rel, alias, parentAlias)
	return "(SELECT " + col + " FROM " + from + " WHERE " + cond + ")", nil
}

// findEmbed returns the embed an order=rel(col) term refers to, matched by the
// embed's alias or, when it has none, its written target name. It mirrors the
// planner's findEmbedByName so the compiler resolves the same relation the
// planner validated.
func (b *builder) findEmbed(name string) *ir.Embed {
	for i := range b.embeds {
		emb := &b.embeds[i]
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
