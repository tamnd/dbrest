package sqlgen

import (
	"fmt"
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
type builder struct {
	d    Dialect
	sb   strings.Builder
	args []any
}

// bind appends a value to the argument list and returns its placeholder.
func (b *builder) bind(v any) string {
	b.args = append(b.args, v)
	return b.d.Placeholder(len(b.args))
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
	b := &builder{d: d}
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
	b := &builder{d: d}
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
	if len(c.Path) != 1 || c.Last != ir.JSONNone {
		return "", pgerr.ErrUnsupported("JSON path projection", "sql")
	}
	expr := b.d.QuoteIdent(c.Path[0])
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
	col := b.d.QuoteIdent(c.Path[0])

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
		col := b.d.QuoteIdent(t.Path[0])
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
