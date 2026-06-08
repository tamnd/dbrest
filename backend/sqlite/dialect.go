// Package sqlite is the reference SQLite backend, built on the pure-Go
// modernc.org/sqlite driver (cgo-free, so dbrest cross-compiles and the test
// suite runs anywhere). It implements the backend SPI (spec 03) and supplies a
// Dialect to the shared compiler (spec 06). Many SQL features are native; the
// PostgreSQL security model (roles, RLS, GUCs) is emulated in-app.
package sqlite

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// dialect is the SQLite spelling for the shared compiler. SQLite uses
// double-quoted identifiers, positional ? placeholders, and (on 3.30+) native
// NULLS FIRST/LAST, so most methods are near-passthrough to PostgreSQL.
type dialect struct{}

// QuoteIdent double-quotes an identifier, doubling any embedded quote.
func (dialect) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Placeholder renders a positional placeholder. SQLite numbers ?n from 1, which
// matches the compiler's 1-based positions and keeps statements readable.
func (dialect) Placeholder(n int) string { return "?" + strconv.Itoa(n) }

// LimitOffset emits LIMIT/OFFSET. SQLite needs no ORDER BY for paging, so
// hasOrder is unused. Either bound is omittable; OFFSET without LIMIT uses the
// SQLite idiom LIMIT -1.
func (dialect) LimitOffset(limit, offset *int, _ bool) string {
	switch {
	case limit == nil && offset == nil:
		return ""
	case offset == nil:
		return "LIMIT " + strconv.Itoa(*limit)
	case limit == nil:
		return "LIMIT -1 OFFSET " + strconv.Itoa(*offset)
	default:
		return "LIMIT " + strconv.Itoa(*limit) + " OFFSET " + strconv.Itoa(*offset)
	}
}

// NullsOrder uses SQLite's native NULLS FIRST/LAST (3.30.0+) to match
// PostgreSQL: NULLS LAST on ASC, NULLS FIRST on DESC, unless the client asked
// otherwise. No synthetic sort key is needed.
func (dialect) NullsOrder(col, dir string, desc bool, nullsFirst *bool) (string, string) {
	first := desc // PG default: NULLS FIRST on DESC, NULLS LAST on ASC
	if nullsFirst != nil {
		first = *nullsFirst
	}
	nulls := "NULLS LAST"
	if first {
		nulls = "NULLS FIRST"
	}
	return "", col + " " + dir + " " + nulls
}

// Returning emits a RETURNING clause (SQLite 3.35.0+).
func (dialect) Returning(cols []string) (string, bool) {
	if len(cols) == 0 {
		return "", false
	}
	return "RETURNING " + strings.Join(cols, ", "), true
}

// Upsert builds ON CONFLICT (cols) DO UPDATE/NOTHING (SQLite 3.24.0+).
func (dialect) Upsert(spec sqlgen.UpsertSpec) (string, error) {
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

// JSONObject assembles a JSON object with json_object (JSON1).
func (dialect) JSONObject(pairs []sqlgen.Pair) string {
	parts := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		parts = append(parts, "'"+strings.ReplaceAll(p.Key, "'", "''")+"'", p.Value)
	}
	return "json_object(" + strings.Join(parts, ", ") + ")"
}

// JSONAgg aggregates rows with json_group_array. SQLite's aggregate does not
// take an ORDER BY argument; ordering is applied on the subquery feeding it.
func (dialect) JSONAgg(elem, _ string) string {
	return "json_group_array(" + elem + ")"
}

// Cast translates a canonical type to SQLite's affinity-based spelling.
func (dialect) Cast(expr, canonicalType string) string {
	switch canonicalType {
	case "int", "int2", "int4", "int8", "bigint", "smallint", "integer":
		return "CAST(" + expr + " AS INTEGER)"
	case "numeric", "decimal", "real", "float", "float4", "float8", "double precision":
		return "CAST(" + expr + " AS REAL)"
	case "text", "varchar", "char", "date", "timestamp", "timestamptz", "uuid":
		return "CAST(" + expr + " AS TEXT)"
	case "json", "jsonb":
		return "json(" + expr + ")"
	case "bool", "boolean":
		// SQLite has no boolean affinity; coerce through INTEGER.
		return "CAST(" + expr + " AS INTEGER)"
	default:
		return "CAST(" + expr + " AS TEXT)"
	}
}

// Regex renders a REGEXP match. The dbrest backend registers a regexp() function
// over Go's regexp on every connection; imatch compiles case-insensitively via a
// (?i) prefix the function honors. The pattern is bound: the returned fragment
// carries the sqlgen.PatternMark sentinel where the bound placeholder goes (a
// literal ? is avoided because the (?i) prefix already contains one).
func (dialect) Regex(expr, _ string, ci bool) (string, bool) {
	if ci {
		return "regexp('(?i)' || " + sqlgen.PatternMark + ", " + expr + ")", true
	}
	return "regexp(" + sqlgen.PatternMark + ", " + expr + ")", true
}

// SessionRead and SessionWrite report no engine-side setting store. SQLite has
// no value a query can read mid-statement the way current_setting (PostgreSQL)
// or SESSION_CONTEXT (SQL Server) can, so the request context is not pushed to
// the engine at all: the specific values a policy references are bound as
// parameters when the predicate is injected into the IR (spec 14/15). The empty
// forms tell the compiler there is nothing to read or write, so it emits no
// setting statement. See spec 15, "MySQL, SQLite, MongoDB: emulated".
func (dialect) SessionRead(string) string { return "" }

// SessionWrite reports ok=false: there is no engine setting to write.
func (dialect) SessionWrite(string) (string, bool) { return "", false }

// BoolValue renders a boolean as 1/0; SQLite has no native boolean.
func (dialect) BoolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// ILike uses plain LIKE; SQLite LIKE is case-insensitive for ASCII.
func (dialect) ILike(col, val string) (string, bool) { return col + " LIKE " + val, true }
