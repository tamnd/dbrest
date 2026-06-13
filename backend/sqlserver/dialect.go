// Package sqlserver is the SQL Server (T-SQL) dialect for the shared compiler
// (spec 06). SQL Server is closer to PostgreSQL on the security model, with
// native roles, row-level security, and a session-context store, but its SQL
// syntax has the most friction of the four engines: bracket-quoted identifiers,
// named @pN placeholders, paging that requires an ORDER BY, no NULLS keyword (a
// CASE expression stands in), no boolean type (the BIT 1/0), OUTPUT in place of
// RETURNING, and a multi-statement upsert (MERGE is avoided for its concurrency
// hazards). This package supplies the Dialect spellings and the
// version-computed Capabilities; the driver-facing half (Execute, introspection,
// and the write-statement assembly that positions OUTPUT and drives the
// multi-statement upsert in a transaction) is a separate slice that needs a live
// server to test.
package sqlserver

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// Dialect is the SQL Server spelling for the shared compiler. It is exported so
// the conformance harness and the future driver-backed backend can hand the same
// value to sqlgen.Compile*, and so the snapshot tests can drive it directly. It
// is stateless: the version-gated behavior (regex availability, the JSON
// assembly form) lives in Capabilities, which the planner consults before the
// compiler ever calls a version-sensitive method.
type Dialect struct{}

// QuoteIdent bracket-quotes an identifier, doubling any embedded closing bracket.
// SQL Server brackets quote a name regardless of the QUOTED_IDENTIFIER setting,
// so dbrest emits them unconditionally.
func (Dialect) QuoteIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// Placeholder renders a named @pN placeholder. go-mssqldb binds each as
// sql.Named("pN", v); the name is what lets a value be referenced more than once
// (the multi-statement upsert reuses the key value in both the UPDATE and the
// INSERT), unlike the positional placeholders of the other engines.
func (Dialect) Placeholder(n int) string { return "@p" + strconv.Itoa(n) }

// LimitOffset emits the OFFSET ... FETCH paging clause. SQL Server requires an
// ORDER BY for OFFSET/FETCH, so when the client gave none and a window is
// present the dialect injects ORDER BY (SELECT 1), a constant order that is
// syntactically valid and leaves the row order to the engine (the same role the
// other engines fill with no ORDER BY at all). FETCH requires an OFFSET, so a
// bare limit pages from OFFSET 0; TOP (n) would avoid the offset but must sit in
// the SELECT list, which this suffix-positioned seam cannot reach, so OFFSET 0 is
// used uniformly.
func (Dialect) LimitOffset(limit, offset *int, hasOrder bool) string {
	if limit == nil && offset == nil {
		if hasOrder {
			// ORDER BY in a derived table requires OFFSET even when no paging is
			// requested; OFFSET 0 ROWS keeps all rows while making the ORDER BY valid.
			return "OFFSET 0 ROWS"
		}
		return ""
	}
	off := 0
	if offset != nil {
		off = *offset
	}
	clause := "OFFSET " + strconv.Itoa(off) + " ROWS"
	if limit != nil {
		clause += " FETCH NEXT " + strconv.Itoa(*limit) + " ROWS ONLY"
	}
	if !hasOrder {
		clause = "ORDER BY (SELECT 1) " + clause
	}
	return clause
}

// NullsOrder reproduces PostgreSQL NULL placement with a CASE sort key, because
// SQL Server has no NULLS keyword. The key ranks NULLs after non-NULLs for NULLS
// LAST and before them for NULLS FIRST; the column's own term carries the
// direction. The PG default is NULLS LAST on ASC and NULLS FIRST on DESC,
// overridable by the client.
func (Dialect) NullsOrder(col, dir string, desc bool, nullsFirst *bool) (string, string) {
	first := desc
	if nullsFirst != nil {
		first = *nullsFirst
	}
	// NULLS LAST: a NULL ranks 1 (sorts after the 0 non-NULLs). NULLS FIRST flips
	// the ranks so a NULL ranks 0.
	nullRank, nonNullRank := "1", "0"
	if first {
		nullRank, nonNullRank = "0", "1"
	}
	sortKey := "CASE WHEN " + col + " IS NULL THEN " + nullRank + " ELSE " + nonNullRank + " END"
	return sortKey, col + " " + dir
}

// Returning emits the OUTPUT clause naming the written columns from the INSERTED
// pseudo-table. The compiler appends this where it appends a trailing RETURNING,
// but T-SQL positions OUTPUT between the column list and VALUES; placing it
// correctly is the write-statement assembly the data-plane slice owns, so this
// fragment supplies the column list and the data plane positions it. A table
// with triggers forces OUTPUT INTO a table variable, which the data plane
// detects from the schema (spec 06).
func (Dialect) Returning(cols []string) (string, bool) {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "INSERTED." + c
	}
	return "OUTPUT " + strings.Join(parts, ", "), true
}

// Upsert reports that SQL Server has no single-statement upsert this seam can
// build. The safe pattern is multi-statement (UPDATE WITH (UPDLOCK, HOLDLOCK)
// ... ; IF @@ROWCOUNT = 0 INSERT ...) inside the request transaction, which the
// data plane composes from the compiled UPDATE and INSERT; MERGE is deliberately
// not used for its documented concurrency hazards. Returning an error here keeps
// the single-statement compiler from emitting a wrong or unsafe upsert (the
// never-silently-wrong rule, spec 06); the capability matrix reports Upsert as
// Emulated so the planner routes a request to the data plane's multi-statement
// path rather than rejecting it.
func (Dialect) Upsert(sqlgen.UpsertSpec) (string, error) {
	return "", errUpsertMultiStatement
}

// errUpsertMultiStatement signals that an upsert cannot be a single statement on
// SQL Server and must be driven by the data plane as a transaction.
var errUpsertMultiStatement = sqlServerError("SQL Server upsert is a multi-statement transaction the data plane drives, not a single-statement clause")

type sqlServerError string

func (e sqlServerError) Error() string { return string(e) }

// JSONObject assembles a JSON object with the SQL Server 2022 JSON_OBJECT
// constructor, whose key/value separator is a colon (unlike the comma of MySQL
// and PostgreSQL). On a server below 2022 the planner reports JSON assembly
// Emulated and the embed path falls back to a FOR JSON PATH subquery, which the
// data-plane slice assembles; this function is the native form.
func (Dialect) JSONObject(pairs []sqlgen.Pair) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, "'"+strings.ReplaceAll(p.Key, "'", "''")+"': "+p.Value)
	}
	return "JSON_OBJECT(" + strings.Join(parts, ", ") + ")"
}

// JSONAgg aggregates rows into a JSON array using STRING_AGG. JSON_ARRAYAGG was
// only added in SQL Server 2025 (version 17); for 2022 compatibility the dialect
// constructs the array manually: '[' + STRING_AGG(elem,',') + ']'. The elements
// are cast to NVARCHAR(MAX) so STRING_AGG accepts them. orderBy is unused; a
// requested embed order is applied on the derived table feeding the aggregate.
func (Dialect) JSONAgg(elem, _ string) string {
	return "'['+STRING_AGG(CAST((" + elem + ") AS NVARCHAR(MAX)),',')+']'"
}

// Cast translates a canonical type to a T-SQL CAST target. SQL Server has no
// AS TEXT (text maps to NVARCHAR(MAX)), a BIT for booleans, DATETIME2 for
// timestamps, UNIQUEIDENTIFIER for uuid, and (before the 2025 JSON type)
// NVARCHAR(MAX) for json.
func (Dialect) Cast(expr, canonicalType string) string {
	return "CAST(" + expr + " AS " + sqlServerType(canonicalType) + ")"
}

// sqlServerType maps a canonical type name to a T-SQL type.
func sqlServerType(canonical string) string {
	switch canonical {
	case "smallint", "int2":
		return "SMALLINT"
	case "int", "integer", "int4":
		return "INT"
	case "bigint", "int8":
		return "BIGINT"
	case "numeric", "decimal":
		return "DECIMAL"
	case "real", "float4":
		return "REAL"
	case "double precision", "float", "float8":
		return "FLOAT"
	case "bool", "boolean":
		return "BIT"
	case "uuid":
		return "UNIQUEIDENTIFIER"
	case "date":
		return "DATE"
	case "timestamp", "timestamptz":
		return "DATETIME2"
	default:
		// text/varchar/char/json and any unknown canonical type render as the
		// widest safe character target.
		return "NVARCHAR(MAX)"
	}
}

// Regex renders REGEXP_LIKE, the predicate SQL Server 2025 and current Azure SQL
// add. The pattern is bound (the fragment carries PatternMark); the
// case-insensitive form passes the 'i' match-control argument. The dialect is
// version-agnostic: a stock server with no regex is gated upstream by
// Capabilities.Regex = Unsupported, so this method is reached only where
// REGEXP_LIKE exists, exactly as the MySQL dialect is.
func (Dialect) Regex(expr, _ string, ci bool) (string, bool) {
	if ci {
		return "REGEXP_LIKE(" + expr + ", " + sqlgen.PatternMark + ", 'i')", true
	}
	return "REGEXP_LIKE(" + expr + ", " + sqlgen.PatternMark + ")", true
}

// RegexFeatureGap reports no gap: where REGEXP_LIKE is available its engine
// honors the constructs an RE2-backed dialect must reject, and where it is not
// available the capability gate has already raised PGRST127, so nothing is
// flagged before lowering. The residual flavor differences are the documented
// Best-effort surface (the conformance allowlist, spec 22).
func (Dialect) RegexFeatureGap(string) string { return "" }

// SessionRead reads a request-context value from the native session store with
// SESSION_CONTEXT, which RLS predicate functions can read inside the engine
// (spec 15). The key is one of dbrest's fixed internal setting names, embedded
// as an N'...' literal because the interface carries a bound operand only for a
// written value.
func (Dialect) SessionRead(key string) string {
	return "SESSION_CONTEXT(N'" + strings.ReplaceAll(key, "'", "''") + "')"
}

// SessionWrite writes a request-context value with sp_set_session_context. The
// key is a fixed internal name embedded as an N'...' literal; the value rides
// through PatternMark as a bound parameter. The write is scoped to the session,
// so the data plane pairs it with a reset when returning a pooled connection.
func (Dialect) SessionWrite(key string) (string, bool) {
	return "EXEC sp_set_session_context N'" + strings.ReplaceAll(key, "'", "''") + "', " + sqlgen.PatternMark, true
}

// ArrayOp implements array containment/overlap operators using OPENJSON, which
// parses the JSON array argument and the JSON array column for element-level
// comparisons. val is a bound placeholder (@pN) whose value is a JSON array
// string (converted from PostgreSQL {a,b} syntax by ArrayLiteral).
func (Dialect) ArrayOp(col, op, val, _ string) (string, bool) {
	switch op {
	case "@>":
		// col contains every element of val
		return "NOT EXISTS(SELECT [value] FROM OPENJSON(" + val + ") WHERE [value] NOT IN (SELECT [value] FROM OPENJSON(" + col + ")))", true
	case "<@":
		// every element of col exists in val
		return "NOT EXISTS(SELECT [value] FROM OPENJSON(" + col + ") WHERE [value] NOT IN (SELECT [value] FROM OPENJSON(" + val + ")))", true
	case "&&":
		// at least one element in common
		return "EXISTS(SELECT 1 FROM OPENJSON(" + col + ") a WHERE a.[value] IN (SELECT [value] FROM OPENJSON(" + val + ")))", true
	}
	return "", false
}

// RangeOp declines: SQL Server has no range types, so sl/sr/nxr/nxl/adj are
// PGRST127.
func (Dialect) RangeOp(_, _, _ string) (string, bool) { return "", false }

// IsBool renders "col = 1" or "col = 0" for SQL Server BIT columns. SQL
// Server's IS operator only accepts NULL/UNKNOWN, not integer literals.
func (Dialect) IsBool(col string, v bool) (string, bool) {
	return col + " = " + Dialect{}.BoolValue(v), true
}

// IsUnknown falls back to "col IS NULL"; a BIT boolean column's UNKNOWN state is
// its NULL, so the row set matches.
func (Dialect) IsUnknown(string) (string, bool) { return "", false }

// ILike uses plain LIKE; SQL Server's default collation is case-insensitive.
func (Dialect) ILike(col, val string) (string, bool) { return col + " LIKE " + val, true }

// BoolValue renders a boolean as the BIT literal 1/0. SQL Server has no boolean
// type or TRUE/FALSE keyword.
func (Dialect) BoolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// ArrayLiteral converts a PostgreSQL {a,b} array literal to a JSON array
// ["a","b"] so OPENJSON in ArrayOp can iterate over it.
func (Dialect) ArrayLiteral(pgText string) string {
	s := strings.TrimSpace(pgText)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return pgText
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return "[]"
	}
	parts := strings.Split(inner, ",")
	quoted := make([]string, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "NULL" {
			quoted[i] = "null"
		} else if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			// PostgreSQL double-quote escaping: "foo" is already valid JSON; pass through.
			quoted[i] = p
		} else {
			quoted[i] = `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
		}
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// ArrayArg stores a payload array as its JSON text: SQL Server has no array
// columns, so an nvarchar column holds the array and reads it back as JSON.
// A PostgreSQL {a,b} literal here would corrupt the column.
func (Dialect) ArrayArg(elems []any, _ string) any { return sqlgen.JSONArrayArg(elems) }

// JSONPath reports ok=false so the compiler raises PGRST127. SQL Server expresses
// JSON access through JSON_VALUE/JSON_QUERY rather than ->/->>, and lowering them
// to match PostgREST's typing needs a live server to verify; until then JSON
// paths are an honest capability gap, the per-driver remainder.
func (Dialect) JSONPath(string, []string, bool) (string, bool) { return "", false }
