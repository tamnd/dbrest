package sqlgen

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/schema"
)

// This file lowers resource embedding (spec 09) to SQL. A read carrying embeds
// is compiled as an aliased parent select where each embedded resource is a
// correlated subquery assembled into JSON in the engine, so one round trip
// returns the whole nested document. A to-one embed is a scalar object subquery;
// a to-many embed is an aggregated array; a many-to-many embed crosses its
// junction in the array subquery. An !inner embed additionally restricts the
// parent through an EXISTS predicate.
//
// JSON assembly is engine-spelled through the Dialect: JSONObject builds the row
// object, JSONAgg folds the array, and Cast(expr, "json") re-parses an embedded
// fragment so a nested object or array nests as JSON rather than as a quoted
// string. The compiler never branches on the engine; a backend that assembles
// JSON differently only supplies those three fragments.

// compileReadEmbedded lowers a read whose query carries one or more embeds. The
// parent relation gets the fixed alias t0; embed targets and junctions are
// aliased t1, t2, ... as they are emitted, in a stable left-to-right order.
func compileReadEmbedded(d Dialect, q *ir.Query) (*Statement, *pgerr.APIError) {
	b := newBuilder(d)
	const parentAlias = "t0"

	b.sb.WriteString("SELECT ")
	if err := b.writeEmbeddedSelect(q, parentAlias); err != nil {
		return nil, err
	}

	b.sb.WriteString(" FROM ")
	b.sb.WriteString(b.qualify(q.Relation))
	b.sb.WriteString(" ")
	b.sb.WriteString(parentAlias)

	// The parent WHERE is its own filters AND, for every !inner embed, an EXISTS
	// over the relationship so a parent with no match drops out.
	wrote := false
	if q.Where != nil {
		b.sb.WriteString(" WHERE ")
		b.qual = parentAlias
		b.parentRef = parentAlias
		b.embeds = q.Embeds
		if err := b.writeCond(*q.Where); err != nil {
			return nil, err
		}
		wrote = true
	}
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
		if err := b.writeEmbedExists(emb, parentAlias); err != nil {
			return nil, err
		}
	}

	hasOrder := len(q.Order) > 0
	if hasOrder {
		b.qual = parentAlias
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

// writeEmbeddedSelect emits the parent projection: each plain column qualified
// by the parent alias, and each embed as a JSON subquery aliased to its output
// key. An empty select projects every parent column.
func (b *builder) writeEmbeddedSelect(q *ir.Query, parentAlias string) *pgerr.APIError {
	if len(q.Select) == 0 {
		b.sb.WriteString(parentAlias + ".*")
		return nil
	}
	first := true
	sep := func() {
		if !first {
			b.sb.WriteString(", ")
		}
		first = false
	}
	for _, it := range q.Select {
		switch v := it.(type) {
		case ir.Column:
			b.qual = parentAlias
			expr, err := b.columnExpr(v)
			if err != nil {
				return err
			}
			sep()
			b.sb.WriteString(expr)
			if name := v.Name(); name != "" && name != lastPath(v.Path) && !isStar(v) {
				b.sb.WriteString(" AS ")
				b.sb.WriteString(b.d.QuoteIdent(name))
			}
		case ir.EmbedRef:
			emb := &q.Embeds[v.Index]
			sub, err := b.embedExpr(emb, parentAlias)
			if err != nil {
				return err
			}
			sep()
			b.sb.WriteString(sub)
			b.sb.WriteString(" AS ")
			b.sb.WriteString(b.d.QuoteIdent(emb.OutKey))
		default:
			return pgerr.ErrUnsupported("aggregates in select", "sql")
		}
	}
	return nil
}

// isStar reports whether a column is the bare `*` projection.
func isStar(c ir.Column) bool {
	return len(c.Path) == 1 && c.Path[0] == "*" && c.Cast == "" && c.Last == ir.JSONNone
}

// embedExpr renders an embed as a self-contained scalar subquery string. It
// captures into a temporary buffer so the result can be nested inside a parent
// JSON object; argument binding stays on the shared counter, so placeholders in
// the returned string line up once it is spliced into the outer SQL.
func (b *builder) embedExpr(emb *ir.Embed, parentAlias string) (string, *pgerr.APIError) {
	return b.capture(func() *pgerr.APIError { return b.writeEmbed(emb, parentAlias) })
}

// writeEmbed lowers one embed into the current buffer. A to-one embed is a
// single-object subquery; a to-many embed aggregates objects into an array,
// crossing the junction first when the relationship is many-to-many.
func (b *builder) writeEmbed(emb *ir.Embed, parentAlias string) *pgerr.APIError {
	if emb.Spread {
		return pgerr.ErrUnsupported("spread embeds", "sql")
	}
	rel := emb.Rel
	b.aliasN++
	alias := "t" + strconv.Itoa(b.aliasN)

	obj, err := b.embedObject(emb, alias)
	if err != nil {
		return err
	}
	from := b.qualify(ir.Ref{Schema: rel.Target.Schema, Name: rel.Target.Name}) + " " + alias

	if rel.Card == schema.CardToOne {
		b.sb.WriteString("(SELECT ")
		b.sb.WriteString(obj)
		b.sb.WriteString(" FROM ")
		b.sb.WriteString(from)
		b.sb.WriteString(" WHERE ")
		b.sb.WriteString(b.joinCond(alias, rel.Foreign, parentAlias, rel.Local))
		if err := b.writeEmbedFilter(emb, alias); err != nil {
			return err
		}
		lim := 1
		if lo := b.d.LimitOffset(&lim, nil, false); lo != "" {
			b.sb.WriteString(" ")
			b.sb.WriteString(lo)
		}
		b.sb.WriteString(")")
		return nil
	}

	// to-many: aggregate per-row objects into a JSON array. COALESCE wraps the
	// aggregate so that a parent row with no matching children returns [] instead
	// of NULL, matching PostgREST's behavior. The empty-array literal is cast via
	// the dialect so it works across engines (PG: '[]'::json, SQLite: json('[]')).
	eAlias := b.d.QuoteIdent("__e")
	b.sb.WriteString("(SELECT COALESCE(")
	b.sb.WriteString(b.d.JSONAgg(b.d.Cast("je."+eAlias, "json"), ""))
	b.sb.WriteString(", ")
	b.sb.WriteString(b.d.Cast("'[]'", "json"))
	b.sb.WriteString(")")
	b.sb.WriteString(" FROM (SELECT ")
	b.sb.WriteString(obj)
	b.sb.WriteString(" AS " + eAlias + " FROM ")
	b.sb.WriteString(from)
	if rel.Junction != nil {
		jx := "j" + strconv.Itoa(b.aliasN)
		jfrom := b.qualify(ir.Ref{Schema: rel.Junction.Schema, Name: rel.Junction.Name}) + " " + jx
		b.sb.WriteString(" JOIN ")
		b.sb.WriteString(jfrom)
		b.sb.WriteString(" ON ")
		b.sb.WriteString(b.joinCond(jx, rel.JForeign, alias, rel.Foreign))
		b.sb.WriteString(" WHERE ")
		b.sb.WriteString(b.joinCond(jx, rel.JLocal, parentAlias, rel.Local))
	} else {
		b.sb.WriteString(" WHERE ")
		b.sb.WriteString(b.joinCond(alias, rel.Foreign, parentAlias, rel.Local))
	}
	if err := b.writeEmbedFilter(emb, alias); err != nil {
		return err
	}
	hasOrder := len(emb.Query.Order) > 0
	if hasOrder {
		saved := b.qual
		b.qual = alias
		if err := b.writeOrder(emb.Query.Order); err != nil {
			b.qual = saved
			return err
		}
		b.qual = saved
	}
	if clause := b.d.LimitOffset(emb.Query.Limit, emb.Query.Offset, hasOrder); clause != "" {
		b.sb.WriteString(" ")
		b.sb.WriteString(clause)
	}
	b.sb.WriteString(") je)")
	return nil
}

// writeEmbedExists emits the EXISTS predicate that an !inner embed adds to the
// parent WHERE, so a parent row with no embedded match is excluded. The same
// embedded filters apply, matching PostgREST's inner-join semantics.
func (b *builder) writeEmbedExists(emb *ir.Embed, parentAlias string) *pgerr.APIError {
	rel := emb.Rel
	b.aliasN++
	alias := "x" + strconv.Itoa(b.aliasN)
	from := b.qualify(ir.Ref{Schema: rel.Target.Schema, Name: rel.Target.Name}) + " " + alias

	b.sb.WriteString("EXISTS (SELECT 1 FROM ")
	b.sb.WriteString(from)
	if rel.Junction != nil {
		jx := "xj" + strconv.Itoa(b.aliasN)
		jfrom := b.qualify(ir.Ref{Schema: rel.Junction.Schema, Name: rel.Junction.Name}) + " " + jx
		b.sb.WriteString(" JOIN ")
		b.sb.WriteString(jfrom)
		b.sb.WriteString(" ON ")
		b.sb.WriteString(b.joinCond(jx, rel.JForeign, alias, rel.Foreign))
		b.sb.WriteString(" WHERE ")
		b.sb.WriteString(b.joinCond(jx, rel.JLocal, parentAlias, rel.Local))
	} else {
		b.sb.WriteString(" WHERE ")
		b.sb.WriteString(b.joinCond(alias, rel.Foreign, parentAlias, rel.Local))
	}
	if err := b.writeEmbedFilter(emb, alias); err != nil {
		return err
	}
	b.sb.WriteString(")")
	return nil
}

// embedObject builds the JSON object for one embedded row: a key/value pair per
// projected column (qualified by the target alias), with nested embeds rendered
// as JSON-typed subqueries. An empty or star projection takes every column of the
// target.
func (b *builder) embedObject(emb *ir.Embed, alias string) (string, *pgerr.APIError) {
	var pairs []Pair
	addAll := func() {
		for _, name := range emb.Rel.Target.ColumnNames() {
			pairs = append(pairs, Pair{Key: name, Value: alias + "." + b.d.QuoteIdent(name)})
		}
	}
	if len(emb.Query.Select) == 0 {
		addAll()
		return b.d.JSONObject(pairs), nil
	}

	saved := b.qual
	b.qual = alias
	defer func() { b.qual = saved }()

	for _, it := range emb.Query.Select {
		switch v := it.(type) {
		case ir.Column:
			if isStar(v) {
				addAll()
				continue
			}
			expr, err := b.columnExpr(v)
			if err != nil {
				return "", err
			}
			pairs = append(pairs, Pair{Key: v.Name(), Value: expr})
		case ir.EmbedRef:
			nested := &emb.Query.Embeds[v.Index]
			sub, err := b.embedExpr(nested, alias)
			if err != nil {
				return "", err
			}
			pairs = append(pairs, Pair{Key: nested.OutKey, Value: b.d.Cast(sub, "json")})
		case ir.Aggregate:
			if v.Func == ir.AggCount && v.Arg == nil {
				pairs = append(pairs, Pair{Key: "count", Value: "count(*)"})
			}
		default:
			return "", pgerr.ErrUnsupported("aggregates in embedded resources", "sql")
		}
	}
	return b.d.JSONObject(pairs), nil
}

// writeEmbedFilter appends the embed's own horizontal filters, ANDed onto the
// join predicate and qualified by the target alias.
func (b *builder) writeEmbedFilter(emb *ir.Embed, alias string) *pgerr.APIError {
	if emb.Query.Where == nil {
		return nil
	}
	saved := b.qual
	b.qual = alias
	b.sb.WriteString(" AND (")
	err := b.writeCond(*emb.Query.Where)
	b.sb.WriteString(")")
	b.qual = saved
	return err
}

// joinCond renders an equi-join between two aliased relations: leftAlias.leftCols
// = rightAlias.rightCols, ANDed across a composite key.
func (b *builder) joinCond(leftAlias string, leftCols []string, rightAlias string, rightCols []string) string {
	parts := make([]string, len(leftCols))
	for i := range leftCols {
		parts[i] = leftAlias + "." + b.d.QuoteIdent(leftCols[i]) +
			" = " + rightAlias + "." + b.d.QuoteIdent(rightCols[i])
	}
	return strings.Join(parts, " AND ")
}
