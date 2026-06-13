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
	"fmt"
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
	// DO UPDATE without a conflict target is not valid PostgreSQL ("ON CONFLICT DO
	// UPDATE requires inference specification or constraint name"). The compiler
	// already degrades a no-target upsert to a plain INSERT, so this guards against
	// a future caller emitting the invalid form.
	if !spec.Ignore && len(spec.Update) > 0 && len(spec.Target) == 0 {
		return "", fmt.Errorf("merge upsert needs a conflict target")
	}
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
//
// PostgreSQL caps a function call at 100 arguments (FUNC_MAX_ARGS), and each pair
// is two arguments, so an object of more than 50 keys (a wide embedded table)
// would raise 54023. Past that threshold the object is built in chunks of 50
// pairs with jsonb_build_object and concatenated with jsonb's || , then cast back
// to json so the result type matches the unchunked form for json_agg and the
// json cast downstream.
func (Dialect) JSONObject(pairs []sqlgen.Pair) string {
	const maxPairs = 50
	buildChunk := func(chunk []sqlgen.Pair, fn string) string {
		parts := make([]string, 0, len(chunk)*2)
		for _, p := range chunk {
			parts = append(parts, "'"+strings.ReplaceAll(p.Key, "'", "''")+"'", p.Value)
		}
		return fn + "(" + strings.Join(parts, ", ") + ")"
	}
	if len(pairs) <= maxPairs {
		return buildChunk(pairs, "json_build_object")
	}
	var chunks []string
	for i := 0; i < len(pairs); i += maxPairs {
		end := i + maxPairs
		if end > len(pairs) {
			end = len(pairs)
		}
		chunks = append(chunks, buildChunk(pairs[i:end], "jsonb_build_object"))
	}
	return "to_json(" + strings.Join(chunks, " || ") + ")"
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
// expression, not just its tail. The type name is passed through to PostgreSQL
// after the parser has validated it against a safe grammar (ir.validCastType),
// so casts to money, interval, an enum, a domain, or an array type resolve the
// same way they do under PostgREST rather than degrading to text.
func (Dialect) Cast(expr, canonicalType string) string {
	return "(" + expr + ")::" + pgType(canonicalType)
}

// pgType normalizes a handful of canonical aliases to one PostgreSQL spelling
// (int->int4 and friends) and passes every other type name through unchanged.
// The name has already been validated as a safe type spelling by the parser, so
// PostgreSQL resolves it directly the way PostgREST relies on.
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
		return canonical
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
func (Dialect) ArrayOp(col, op, val, _ string) (string, bool) {
	return col + " " + op + " " + val, true
}

// RangeOp renders PostgreSQL's native range operators (<<, >>, &<, &>, -|-).
func (Dialect) RangeOp(col, op, val string) (string, bool) {
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

// IsBool falls back to the generic "IS TRUE"/"IS FALSE" form; PostgreSQL
// supports IS <bool> natively.
func (Dialect) IsBool(string, bool) (string, bool) { return "", false }

// IsUnknown renders PostgreSQL's native three-valued "col IS UNKNOWN" test.
func (Dialect) IsUnknown(col string) (string, bool) { return col + " IS UNKNOWN", true }

// ArrayLiteral returns the PostgreSQL {a,b} array literal unchanged; PostgreSQL
// accepts it natively.
func (Dialect) ArrayLiteral(pgText string) string { return pgText }

// ArrayArg renders a payload array for the target column. A JSON array bound for
// a json/jsonb column is JSON, not a PostgreSQL array, so it is kept as JSON
// text; for an array column it becomes the {a,b} array-literal text so the
// server-side cast from text to text[]/int4[]/etc. succeeds with or without type
// OIDs. An unknown column type keeps the array-literal default.
func (Dialect) ArrayArg(elems []any, colType string) any {
	if colType == "json" || colType == "jsonb" {
		return sqlgen.JSONArrayArg(elems)
	}
	return sqlgen.PGArrayLiteral(elems)
}

// JSONPath emits PostgreSQL's native -> / ->> operator chain: every hop is ->
// (json) except the final one, which is ->> when the access was text. A digit
// segment becomes an integer array index; any other segment is a quoted key.
func (Dialect) JSONPath(base string, hops []string, asText bool) (string, bool) {
	var b strings.Builder
	b.WriteString(base)
	for i, h := range hops {
		op := "->"
		if asText && i == len(hops)-1 {
			op = "->>"
		}
		b.WriteString(op)
		if sqlgen.IsJSONArrayIndex(h) {
			b.WriteString(h)
		} else {
			b.WriteString("'" + strings.ReplaceAll(h, "'", "''") + "'")
		}
	}
	return b.String(), true
}
