package sqlite

import (
	"strings"

	"github.com/tamnd/dbrest/schema"
)

// parseViewColumns traces a SQLite view's output columns back to the base-table
// columns they project, so the schema model can carry the base table's foreign
// keys onto the view (spec 09). SQLite keeps no column-provenance catalog, so the
// CREATE VIEW text from sqlite_master is parsed here.
//
// The parser is deliberately conservative and recognizes only the shape whose
// provenance is unambiguous: a single base relation in the FROM clause, no join,
// no set operation, and a select list of bare or aliased column references. Any
// expression column, function call, join, or UNION yields no mapping, so the
// model inherits nothing rather than guessing, the same way PostgREST skips a
// view it cannot resolve. baseCols returns a base relation's column names, used
// to expand `SELECT *`.
func parseViewColumns(ddl string, baseCols func(name string) ([]string, bool)) []schema.ViewColumn {
	sel, ok := viewSelectBody(ddl)
	if !ok {
		return nil
	}
	// A set operation (UNION, INTERSECT, EXCEPT) makes provenance ambiguous.
	if hasTopLevelKeyword(sel, "union", "intersect", "except") {
		return nil
	}
	listText, fromText, ok := splitSelectFrom(sel)
	if !ok {
		return nil
	}
	base, ok := singleBaseTable(fromText)
	if !ok {
		return nil
	}

	items := splitArgs(listText)
	var out []schema.ViewColumn
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "*" || strings.HasSuffix(item, ".*") {
			cols, ok := baseCols(base)
			if !ok {
				return nil
			}
			for _, c := range cols {
				out = append(out, schema.ViewColumn{Name: c, BaseRelation: base, BaseColumn: c})
			}
			continue
		}
		vc, ok := parseSelectColumn(item, base)
		if !ok {
			return nil // an expression column: do not project this view
		}
		out = append(out, vc)
	}
	return out
}

// viewSelectBody extracts the SELECT body of a CREATE VIEW statement, dropping
// the "CREATE VIEW name AS" prefix. It reports ok=false for any other DDL.
func viewSelectBody(ddl string) (string, bool) {
	low := strings.ToLower(ddl)
	if !strings.Contains(low, "create") || !strings.Contains(low, "view") {
		return "", false
	}
	as := indexWord(low, "as")
	if as < 0 {
		return "", false
	}
	return strings.TrimSpace(ddl[as+2:]), true
}

// splitSelectFrom splits a SELECT body into its select list and FROM clause. It
// drops a leading SELECT (and DISTINCT) and cuts at the top-level FROM keyword,
// reporting ok=false when there is no FROM.
func splitSelectFrom(sel string) (list, from string, ok bool) {
	low := strings.ToLower(sel)
	if !strings.HasPrefix(low, "select") {
		return "", "", false
	}
	sel = strings.TrimSpace(sel[len("select"):])
	if low := strings.ToLower(sel); strings.HasPrefix(low, "distinct") {
		sel = strings.TrimSpace(sel[len("distinct"):])
	}
	at := topLevelKeyword(sel, "from")
	if at < 0 {
		return "", "", false
	}
	list = strings.TrimSpace(sel[:at])
	from = strings.TrimSpace(sel[at+len("from"):])
	return list, from, true
}

// singleBaseTable returns the lone base relation named in a FROM clause, or
// ok=false when the clause has a join, a comma, a subquery, or trailing clauses
// the parser will not reason about (WHERE, GROUP BY, and the rest).
func singleBaseTable(from string) (string, bool) {
	// Cut anything after the table reference: a WHERE/GROUP/ORDER/LIMIT tail.
	for _, kw := range []string{"where", "group", "order", "limit", "having", "window"} {
		if at := topLevelKeyword(from, kw); at >= 0 {
			from = strings.TrimSpace(from[:at])
		}
	}
	if from == "" {
		return "", false
	}
	// A join or a comma-separated list is more than one base relation.
	if strings.Contains(strings.ToLower(from), " join ") || strings.ContainsAny(from, ",(") {
		return "", false
	}
	fields := strings.Fields(from)
	// Accept "base" or "base alias"; reject "base AS alias" forms beyond two words
	// only when they introduce something other than a plain alias.
	if len(fields) == 0 {
		return "", false
	}
	return unquoteIdent(fields[0]), true
}

// parseSelectColumn parses one select-list item that is a bare or aliased column
// reference, returning the view column to base column mapping. It reports
// ok=false for an expression (a function call, an operator, a literal), which the
// caller treats as a reason not to project the view.
func parseSelectColumn(item, base string) (schema.ViewColumn, bool) {
	// Split off an alias: "expr AS name" or "expr name".
	expr, alias := splitColumnAlias(item)
	// The expression must be a plain column reference: an identifier, optionally
	// qualified by a table. Anything with an operator, call, or literal is out.
	if !isColumnRef(expr) {
		return schema.ViewColumn{}, false
	}
	baseCol := expr
	if dot := strings.LastIndexByte(expr, '.'); dot >= 0 {
		baseCol = expr[dot+1:]
	}
	baseCol = unquoteIdent(strings.TrimSpace(baseCol))
	name := baseCol
	if alias != "" {
		name = unquoteIdent(alias)
	}
	return schema.ViewColumn{Name: name, BaseRelation: base, BaseColumn: baseCol}, true
}

// splitColumnAlias separates a select item into its expression and column alias.
// It handles "expr AS alias" and the bare "expr alias" form, and returns an empty
// alias when the item is a single token.
func splitColumnAlias(item string) (expr, alias string) {
	if at := indexWord(strings.ToLower(item), "as"); at >= 0 {
		return strings.TrimSpace(item[:at]), strings.TrimSpace(item[at+2:])
	}
	fields := strings.Fields(item)
	if len(fields) == 2 {
		return fields[0], fields[1]
	}
	return strings.TrimSpace(item), ""
}

// isColumnRef reports whether s is a plain (optionally table-qualified) column
// reference: identifier characters, quotes, and a single dot, with no operator,
// parenthesis, or whitespace that would mark an expression.
func isColumnRef(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '"' || c == '`' || c == '[' || c == ']' || c == '$':
		default:
			return false
		}
	}
	return true
}

// indexWord finds the byte offset of a standalone lowercase word in s (which the
// caller has already lowercased where needed), requiring word boundaries so that
// "as" does not match inside "class". It returns -1 when absent.
func indexWord(s, word string) int {
	from := 0
	for {
		at := strings.Index(s[from:], word)
		if at < 0 {
			return -1
		}
		at += from
		if wordBoundary(s, at, len(word)) {
			return at
		}
		from = at + len(word)
	}
}

// topLevelKeyword finds a standalone keyword in s outside any parentheses or
// quotes, the boundary a clause splitter needs so a keyword inside a subquery or
// string does not match. It matches case-insensitively and returns -1 when absent.
func topLevelKeyword(s, keyword string) int {
	low := strings.ToLower(s)
	depth := 0
	var quote byte
	for i := 0; i < len(low); i++ {
		c := low[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case depth == 0 && c == keyword[0] && strings.HasPrefix(low[i:], keyword) && wordBoundary(low, i, len(keyword)):
			return i
		}
	}
	return -1
}

// hasTopLevelKeyword reports whether any of the keywords appears at the top level.
func hasTopLevelKeyword(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if topLevelKeyword(s, kw) >= 0 {
			return true
		}
	}
	return false
}

// wordBoundary reports whether the substring at [at, at+n) in s is bounded by
// non-identifier characters on both sides.
func wordBoundary(s string, at, n int) bool {
	if at > 0 && isIdentByte(s[at-1]) {
		return false
	}
	end := at + n
	if end < len(s) && isIdentByte(s[end]) {
		return false
	}
	return true
}

// isIdentByte reports whether c can appear inside an unquoted SQL identifier.
func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
