package sqlite

import (
	"context"
	"sort"
	"strings"

	"github.com/tamnd/dbrest/schema"
)

// Introspect builds the unified schema model from SQLite's catalogs. Tables and
// views come from sqlite_master; columns and primary keys from PRAGMA
// table_info. SQLite's single main database maps to the unqualified namespace
// (empty schema); attached databases become named schemas when that subsystem
// lands. See spec 08.
func (b *Backend) Introspect(ctx context.Context) (*schema.Model, error) {
	rels, err := b.relationNames(ctx)
	if err != nil {
		return nil, err
	}
	// FTS5 virtual tables are not exposed as relations; they are full-text indexes
	// attached to the base table they shadow (external content) or, when standalone,
	// to themselves. Their auto-created shadow tables (_data, _idx, ...) are hidden
	// too. See spec 21, "index requirements".
	ftsByContent, excluded := classifyFTS(rels)

	out := make([]*schema.Relation, 0, len(rels))
	colsByName := make(map[string][]*schema.Column, len(rels))
	ddlByName := make(map[string]string, len(rels))
	for _, r := range rels {
		if excluded[r.name] {
			continue
		}
		cols, pk, err := b.columns(ctx, r.name)
		if err != nil {
			return nil, err
		}
		fks, err := b.foreignKeys(ctx, r.name)
		if err != nil {
			return nil, err
		}
		uniq, err := b.uniques(ctx, r.name)
		if err != nil {
			return nil, err
		}
		colsByName[r.name] = cols
		ddlByName[r.name] = r.sql
		out = append(out, &schema.Relation{
			Name:        r.name,
			Kind:        r.kind,
			Columns:     cols,
			PrimaryKey:  pk,
			Unique:      uniq,
			ForeignKeys: fks,
			FullText:    ftsByContent[r.name],
		})
	}
	// Second pass: parse each view's definition into the base-column mapping the
	// model projects foreign keys through (spec 09). It runs after the first pass
	// so a view referencing a base table defined later in the catalog still
	// resolves. The parser is conservative: it maps only the views it can trace to
	// plain base columns and leaves the rest empty, so the model inherits nothing
	// where provenance is uncertain, the same as PostgREST skips a UNION.
	baseCols := func(name string) ([]string, bool) {
		cols, ok := colsByName[name]
		if !ok {
			return nil, false
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return names, true
	}
	for _, r := range out {
		if r.Kind == schema.KindView {
			r.ViewColumns = parseViewColumns(ddlByName[r.Name], baseCols)
		}
	}
	return schema.NewModel(out), nil
}

type relInfo struct {
	name string
	kind schema.Kind
	sql  string
}

func (b *Backend) relationNames(ctx context.Context) ([]relInfo, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT name, type, COALESCE(sql, '') FROM sqlite_master
		 WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []relInfo
	for rows.Next() {
		var name, typ, ddl string
		if err := rows.Scan(&name, &typ, &ddl); err != nil {
			return nil, err
		}
		kind := schema.KindTable
		if typ == "view" {
			kind = schema.KindView
		}
		out = append(out, relInfo{name: name, kind: kind, sql: ddl})
	}
	return out, rows.Err()
}

// ftsShadowSuffixes are the auto-created backing tables of an FTS5 virtual table,
// named <fts><suffix>. They are real tables in sqlite_master but are internal, so
// they are hidden from the exposed schema.
var ftsShadowSuffixes = []string{"_data", "_idx", "_content", "_docsize", "_config"}

// classifyFTS scans the catalog for FTS5 virtual tables and returns the full-text
// indexes keyed by the base relation they cover, plus the set of relation names to
// hide from the exposed schema (external-content FTS5 tables and every FTS5 table's
// shadow tables). A standalone FTS5 table (no content= option) stays exposed and
// indexes itself.
func classifyFTS(rels []relInfo) (map[string][]*schema.FullTextIndex, map[string]bool) {
	byContent := map[string][]*schema.FullTextIndex{}
	excluded := map[string]bool{}
	for _, r := range rels {
		decl, ok := parseFTS5(r.name, r.sql)
		if !ok {
			continue
		}
		target := decl.content
		if target == "" {
			target = decl.name // standalone: indexes its own columns
		} else {
			excluded[decl.name] = true // external content: the vtab itself is hidden
		}
		byContent[target] = append(byContent[target], &schema.FullTextIndex{
			Name:        decl.name,
			Columns:     decl.columns,
			RowidColumn: decl.rowidCol,
		})
		for _, suf := range ftsShadowSuffixes {
			excluded[decl.name+suf] = true
		}
	}
	return byContent, excluded
}

// ftsDecl is a parsed FTS5 CREATE VIRTUAL TABLE statement.
type ftsDecl struct {
	name     string
	columns  []string // indexed column names, in declared order
	content  string   // external content table, "" when standalone
	rowidCol string   // content_rowid column, "" for the implicit rowid
}

// ftsOptionKeys are the FTS5 configuration arguments that are not columns. Any
// other parenthesized item is an indexed column name.
var ftsOptionKeys = map[string]bool{
	"content": true, "content_rowid": true, "tokenize": true,
	"prefix": true, "columnsize": true, "detail": true,
}

// parseFTS5 recognizes a CREATE VIRTUAL TABLE ... USING fts5(...) statement and
// extracts its indexed columns and the content/content_rowid options. It reports
// ok=false for any other DDL. The sql comes from sqlite_master, a trusted catalog.
func parseFTS5(name, sql string) (ftsDecl, bool) {
	low := strings.ToLower(sql)
	if !strings.Contains(low, "virtual table") || !strings.Contains(low, "fts5") {
		return ftsDecl{}, false
	}
	open := strings.IndexByte(sql, '(')
	close := strings.LastIndexByte(sql, ')')
	if open < 0 || close < open {
		return ftsDecl{}, false
	}
	decl := ftsDecl{name: name}
	for _, arg := range splitArgs(sql[open+1 : close]) {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		if key, val, ok := splitOption(arg); ok {
			switch key {
			case "content":
				decl.content = val
			case "content_rowid":
				decl.rowidCol = val
			}
			continue
		}
		decl.columns = append(decl.columns, ftsColumnName(arg))
	}
	return decl, true
}

// splitOption splits an FTS5 option of the form key=value when key is a known
// option name; it reports ok=false for a column spec. The value is unquoted.
func splitOption(arg string) (key, val string, ok bool) {
	lhs, rhs, found := strings.Cut(arg, "=")
	if !found {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(lhs))
	if !ftsOptionKeys[key] {
		return "", "", false
	}
	return key, unquoteIdent(strings.TrimSpace(rhs)), true
}

// ftsColumnName extracts the column name from an FTS5 column spec, dropping a
// trailing UNINDEXED keyword and any surrounding quoting.
func ftsColumnName(spec string) string {
	if i := strings.IndexAny(spec, " \t"); i >= 0 {
		spec = spec[:i]
	}
	return unquoteIdent(spec)
}

// unquoteIdent strips one layer of SQLite identifier or string quoting: "x", 'x',
// `x`, or [x]. A bare word is returned unchanged.
func unquoteIdent(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	switch {
	case first == '"' && last == '"',
		first == '\'' && last == '\'',
		first == '`' && last == '`':
		return strings.ReplaceAll(s[1:len(s)-1], string(first)+string(first), string(first))
	case first == '[' && last == ']':
		return s[1 : len(s)-1]
	}
	return s
}

// splitArgs splits an FTS5 argument list on top-level commas, ignoring commas
// inside quotes or parentheses (a tokenize='porter unicode61' value can contain
// neither, but nested parens guard against future option forms).
func splitArgs(s string) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// columns reads PRAGMA table_info for one relation, returning its columns in
// ordinal order and the primary-key column names in key order.
func (b *Backend) columns(ctx context.Context, table string) ([]*schema.Column, []string, error) {
	// PRAGMA does not accept a bind parameter for the table name; the name comes
	// from sqlite_master (trusted catalog), not from user input, so it is safe to
	// quote and inline.
	rows, err := b.db.QueryContext(ctx, `PRAGMA table_info(`+dialect{}.QuoteIdent(table)+`)`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type pkEntry struct {
		name string
		ord  int
	}
	var cols []*schema.Column
	var pks []pkEntry
	for rows.Next() {
		var (
			cid       int
			name      string
			declType  string
			notNull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dfltValue, &pk); err != nil {
			return nil, nil, err
		}
		cols = append(cols, &schema.Column{
			Name:       name,
			Type:       canonicalType(declType),
			Nullable:   notNull == 0,
			HasDefault: dfltValue != nil,
			Position:   cid + 1,
		})
		if pk > 0 {
			pks = append(pks, pkEntry{name: name, ord: pk})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	sort.Slice(pks, func(i, j int) bool { return pks[i].ord < pks[j].ord })
	pkNames := make([]string, len(pks))
	for i, p := range pks {
		pkNames[i] = p.name
	}
	return cols, pkNames, nil
}

// foreignKeys reads PRAGMA foreign_key_list for one relation and groups it into
// composite foreign keys. SQLite reports one row per referencing column, keyed by
// an id that ties a composite key together and a seq that orders its columns; a
// NULL "to" means the key references the parent's primary key, which is resolved
// here. SQLite keys carry no name, so a stable one is synthesized as
// {child}_{from-columns}_fkey, matching the introspection contract (spec 08).
func (b *Backend) foreignKeys(ctx context.Context, table string) ([]*schema.ForeignKey, error) {
	rows, err := b.db.QueryContext(ctx, `PRAGMA foreign_key_list(`+dialect{}.QuoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type fkRow struct {
		seq      int
		from, to string
		toNull   bool
	}
	groups := make(map[int]*struct {
		refTable string
		cols     []fkRow
	})
	var order []int
	for rows.Next() {
		var (
			id, seq           int
			refTable, from    string
			to                any
			onUpd, onDel, mat string
		)
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpd, &onDel, &mat); err != nil {
			return nil, err
		}
		g, ok := groups[id]
		if !ok {
			g = &struct {
				refTable string
				cols     []fkRow
			}{refTable: refTable}
			groups[id] = g
			order = append(order, id)
		}
		fr := fkRow{seq: seq}
		fr.from = from
		if s, ok := toString(to); ok {
			fr.to = s
		} else {
			fr.toNull = true
		}
		g.cols = append(g.cols, fr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Ints(order)
	var out []*schema.ForeignKey
	for _, id := range order {
		g := groups[id]
		sort.Slice(g.cols, func(i, j int) bool { return g.cols[i].seq < g.cols[j].seq })

		fromCols := make([]string, len(g.cols))
		toCols := make([]string, 0, len(g.cols))
		needPK := false
		for i, c := range g.cols {
			fromCols[i] = c.from
			if c.toNull {
				needPK = true
			} else {
				toCols = append(toCols, c.to)
			}
		}
		// A key with NULL targets references the parent's primary key.
		if needPK {
			_, refPK, err := b.columns(ctx, g.refTable)
			if err != nil {
				return nil, err
			}
			toCols = refPK
		}
		out = append(out, &schema.ForeignKey{
			Name:        table + "_" + strings.Join(fromCols, "_") + "_fkey",
			Columns:     fromCols,
			RefRelation: g.refTable,
			RefColumns:  toCols,
		})
	}
	return out, nil
}

// uniques reads the relation's unique constraints from PRAGMA index_list and
// index_info, returning each as a set of column names. Only constraint-backed
// indexes are returned: origin "u" (a UNIQUE table constraint) and origin "c"
// (a CREATE UNIQUE INDEX) when the index is unique. The primary key (origin
// "pk") is omitted because table_info already reports it, and the planner tests
// the primary key separately when deciding one-to-one cardinality (spec 09).
func (b *Backend) uniques(ctx context.Context, table string) ([][]string, error) {
	// The table name comes from sqlite_master, not user input; quote and inline it.
	rows, err := b.db.QueryContext(ctx, `PRAGMA index_list(`+dialect{}.QuoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	type idxInfo struct {
		name   string
		origin string
	}
	var indexes []idxInfo
	for rows.Next() {
		var (
			seq, unique, partial int
			name, origin         string
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		// A partial unique index does not constrain the whole column, so it cannot
		// make a foreign key one-to-one; skip it, as PostgREST does.
		if unique == 1 && origin != "pk" && partial == 0 {
			indexes = append(indexes, idxInfo{name: name, origin: origin})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out [][]string
	for _, idx := range indexes {
		cols, err := b.indexColumns(ctx, idx.name)
		if err != nil {
			return nil, err
		}
		if len(cols) > 0 {
			out = append(out, cols)
		}
	}
	return out, nil
}

// indexColumns reads the column names of one index from PRAGMA index_info, in
// key order. A NULL column name marks an expression index column, which cannot
// participate in a foreign-key match, so such an index is dropped by returning
// no columns for it.
func (b *Backend) indexColumns(ctx context.Context, index string) ([]string, error) {
	rows, err := b.db.QueryContext(ctx, `PRAGMA index_info(`+dialect{}.QuoteIdent(index)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type entry struct {
		seq  int
		name string
	}
	var entries []entry
	for rows.Next() {
		var (
			seqno, cid int
			name       any
		)
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, err
		}
		s, ok := toString(name)
		if !ok {
			return nil, nil // expression column: not usable for a key match
		}
		entries = append(entries, entry{seq: seqno, name: s})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	cols := make([]string, len(entries))
	for i, e := range entries {
		cols[i] = e.name
	}
	return cols, nil
}

// toString coerces a scalar from PRAGMA into a string, reporting false for NULL.
func toString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return "", false
	}
}
