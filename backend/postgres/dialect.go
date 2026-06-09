// Package postgres is the PostgreSQL dialect for the shared compiler (spec 06).
// PostgreSQL is dbrest's reference oracle: the compiler emits almost exactly the
// SQL PostgREST itself emits, so this dialect is near-passthrough and almost
// every capability is Native. The conformance suite (spec 22) runs the same
// request matrix against PostgREST+PG and dbrest+PG expecting byte-identical
// output, which is why the dialect's spellings track PG precisely.
//
// This package supplies the two halves a new SQL engine must bring (spec 06,
// "Adding a fifth SQL engine"): the Dialect spellings here, and the
// version-computed Capabilities in capabilities.go. The plan walk, filter-tree
// lowering, embed assembly, count handling, and parameter accounting are the
// shared compiler's and are reused unchanged. The driver-facing half (Execute
// over pgx, introspection) is a separate slice that needs a live server to test
// and is tracked in the implementation note; this slice is the database-free
// core that spec 06 section 7 prescribes testing by snapshotting emitted SQL.
package postgres

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// Dialect is the PostgreSQL spelling for the shared compiler. It is exported so
// the conformance harness and the (future) pgx backend can hand the same value
// to sqlgen.Compile*, and so the snapshot tests can exercise it directly.
type Dialect struct{}

// QuoteIdent double-quotes an identifier, doubling any embedded quote. This is
// the ANSI form PostgreSQL uses.
func (Dialect) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Placeholder renders a positional $n placeholder. PostgreSQL numbers from 1,
// which matches the compiler's 1-based positions; the driver (pgx) binds by
// position.
func (Dialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

// LimitOffset emits LIMIT/OFFSET. PostgreSQL needs no ORDER BY for paging, so
// hasOrder is unused, and either bound is omittable. The integers are rendered
// literally, not bound: they are parsed integers from the planner, never client
// text, and the Dialect interface gives the compiler's binder no entry here. The
// reference SQLite dialect renders them the same way, so the two stay in step.
func (Dialect) LimitOffset(limit, offset *int, _ bool) string {
	switch {
	case limit == nil && offset == nil:
		return ""
	case offset == nil:
		return "LIMIT " + strconv.Itoa(*limit)
	case limit == nil:
		return "OFFSET " + strconv.Itoa(*offset)
	default:
		return "LIMIT " + strconv.Itoa(*limit) + " OFFSET " + strconv.Itoa(*offset)
	}
}

// NullsOrder uses PostgreSQL's native NULLS FIRST/LAST. The PostgreSQL default
// is NULLS LAST on ASC and NULLS FIRST on DESC; the client can override with
// .nullsfirst/.nullslast. No synthetic sort key is needed, so the first return
// is empty.
func (Dialect) NullsOrder(col, dir string, desc bool, nullsFirst *bool) (string, string) {
	first := desc
	if nullsFirst != nil {
		first = *nullsFirst
	}
	nulls := "NULLS LAST"
	if first {
		nulls = "NULLS FIRST"
	}
	return "", col + " " + dir + " " + nulls
}

// Returning emits a RETURNING clause. PostgreSQL has had it since 8.2, so it is
// always available; ok is false only for an empty column list.
func (Dialect) Returning(cols []string) (string, bool) {
	if len(cols) == 0 {
		return "", false
	}
	return "RETURNING " + strings.Join(cols, ", "), true
}

// Upsert builds ON CONFLICT (cols) DO UPDATE/NOTHING. PostgreSQL honors the
// conflict target, and the excluded pseudo-table carries the would-be-inserted
// row, so a merge sets each column to its excluded value. An empty update set or
// an ignore request becomes DO NOTHING.
func (Dialect) Upsert(spec sqlgen.UpsertSpec) (string, error) {
	var sb strings.Builder
	sb.WriteString("ON CONFLICT")
	if len(spec.Target) > 0 {
		sb.WriteString(" (")
		sb.WriteString(strings.Join(spec.Target, ", "))
		sb.WriteString(")")
	}
	if spec.Ignore || len(spec.Update) == 0 {
		sb.WriteString(" DO NOTHING")
		return sb.String(), nil
	}
	sb.WriteString(" DO UPDATE SET ")
	for i, c := range spec.Update {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(c + " = excluded." + c)
	}
	return sb.String(), nil
}

// JSONObject assembles a JSON object with json_build_object, the function whose
// argument order fixes the key order to the select order (spec 06, "JSON
// assembly"). Keys are JSON string literals; values are already-compiled SQL.
func (Dialect) JSONObject(pairs []sqlgen.Pair) string {
	parts := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		parts = append(parts, "'"+strings.ReplaceAll(p.Key, "'", "''")+"'", p.Value)
	}
	return "json_build_object(" + strings.Join(parts, ", ") + ")"
}

// JSONAgg aggregates rows with json_agg. PostgreSQL takes an ORDER BY inside the
// aggregate, so a requested embed order is honored at the point of aggregation
// rather than on a feeding subquery; when none is given the clause is omitted.
func (Dialect) JSONAgg(elem, orderBy string) string {
	if orderBy == "" {
		return "json_agg(" + elem + ")"
	}
	return "json_agg(" + elem + " ORDER BY " + orderBy + ")"
}

// Cast translates a canonical type to a PostgreSQL ::type cast, the form PG
// itself uses. The expression is parenthesized so the cast binds to the whole
// expression, not just its tail. An unknown canonical type falls back to text,
// which is the safe rendering for an opaque value.
func (Dialect) Cast(expr, canonicalType string) string {
	return "(" + expr + ")::" + pgType(canonicalType)
}

// pgType maps a canonical type name to its PostgreSQL spelling. The canonical
// names are the PG type names already in most cases, so the map mostly
// normalizes aliases (int->int4, bool->boolean stays bool) to one spelling.
func pgType(canonical string) string {
	switch canonical {
	case "int", "integer", "int4":
		return "int4"
	case "int2", "smallint":
		return "int2"
	case "int8", "bigint":
		return "int8"
	case "float4", "real":
		return "float4"
	case "float8", "double precision":
		return "float8"
	case "numeric", "decimal":
		return "numeric"
	case "bool", "boolean":
		return "bool"
	case "text":
		return "text"
	case "varchar", "char":
		return canonical
	case "date":
		return "date"
	case "timestamp":
		return "timestamp"
	case "timestamptz":
		return "timestamptz"
	case "uuid":
		return "uuid"
	case "json":
		return "json"
	case "jsonb":
		return "jsonb"
	default:
		return "text"
	}
}

// Regex renders a POSIX regex match: ~ for case-sensitive (match), ~* for
// case-insensitive (imatch). The pattern is bound, so the fragment carries the
// PatternMark sentinel where the placeholder goes and the compiler substitutes
// the real $n.
func (Dialect) Regex(expr, _ string, ci bool) (string, bool) {
	op := "~"
	if ci {
		op = "~*"
	}
	return expr + " " + op + " " + sqlgen.PatternMark, true
}

// RegexFeatureGap reports no gap. PostgreSQL is the reference oracle: whatever
// its POSIX engine does for a pattern is correct by definition, so no construct
// is rejected ahead of lowering the way an RE2-backed engine's are (spec 21).
func (Dialect) RegexFeatureGap(string) string { return "" }

// SessionRead reads a request-context value with current_setting(key, true).
// The trailing true makes a missing setting return NULL instead of erroring, so
// a policy that references an unset value sees NULL rather than failing the
// query. The key is one of dbrest's fixed internal setting names (not client
// input); it is embedded as a single-quote-escaped string literal because the
// Dialect interface carries a bound value only for the query operand, not for a
// setting name. See spec 15.
func (Dialect) SessionRead(key string) string {
	return "current_setting(" + sqlLiteral(key) + ", true)"
}

// SessionWrite writes a request-context value with set_config(key, val, true).
// The trailing true scopes the write to the current transaction (SET LOCAL), so
// the value does not leak to the next request on a pooled connection. The key is
// embedded as an escaped literal (as in SessionRead); the value rides through as
// the bound operand, so the fragment carries PatternMark where its placeholder
// goes and the compiler binds it. See spec 15.
func (Dialect) SessionWrite(key string) (string, bool) {
	return "set_config(" + sqlLiteral(key) + ", " + sqlgen.PatternMark + ", true)", true
}

// sqlLiteral renders a string as a single-quoted SQL literal, doubling embedded
// quotes. It is used only for dbrest's fixed setting names, never for client
// values (which are always bound).
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ArrayOp renders a PostgreSQL array containment/overlap expression.
func (Dialect) ArrayOp(col, op, val string) (string, bool) {
	return col + " " + op + " " + val, true
}

// ILike renders col ILIKE val using PostgreSQL's native case-insensitive LIKE.
func (Dialect) ILike(col, val string) (string, bool) { return col + " ILIKE " + val, true }

// BoolValue renders a boolean as the PostgreSQL keyword TRUE/FALSE.
func (Dialect) BoolValue(v bool) string {
	if v {
		return "TRUE"
	}
	return "FALSE"
}


