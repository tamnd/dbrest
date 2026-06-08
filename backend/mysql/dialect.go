// Package mysql is the MySQL/MariaDB dialect for the shared compiler (spec 06).
// MySQL diverges from PostgreSQL in several keyword and syntax choices that the
// capability matrix (spec 04) tracks: no NULLS keyword (an explicit IS NULL sort
// key stands in), no per-conflict-target upsert (ON DUPLICATE KEY fires on any
// unique key), no RETURNING on MySQL 8 (the data plane re-selects by key), a
// restricted CAST target set, and no SQL-readable session store (request-context
// values are bound into predicates). This package supplies the Dialect spellings
// and the version-computed Capabilities; the driver-facing half (Execute,
// introspection) is a separate slice that needs a live server to test.
package mysql

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// Dialect is the MySQL/MariaDB spelling for the shared compiler. It is exported
// so the conformance harness and the future driver-backed backend can hand the
// same value to sqlgen.Compile*, and so the snapshot tests can drive it
// directly. It is stateless.
type Dialect struct{}

// QuoteIdent backtick-quotes an identifier, doubling any embedded backtick.
// dbrest emits backticks unconditionally rather than relying on ANSI_QUOTES.
func (Dialect) QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// Placeholder renders a positional ? placeholder. MySQL placeholders are not
// numbered; the driver binds them in append order, which is the order the
// compiler appends arguments, so the bare ? is correct for every position.
func (Dialect) Placeholder(int) string { return "?" }

// LimitOffset emits LIMIT/OFFSET. MySQL needs no ORDER BY for paging, so
// hasOrder is unused. MySQL cannot take OFFSET without LIMIT, so an offset with
// no limit uses the documented idiom LIMIT <max bigint> OFFSET m.
func (Dialect) LimitOffset(limit, offset *int, _ bool) string {
	switch {
	case limit == nil && offset == nil:
		return ""
	case offset == nil:
		return "LIMIT " + strconv.Itoa(*limit)
	case limit == nil:
		// 18446744073709551615 is 2^64-1, the MySQL idiom for "no limit" so an
		// OFFSET can stand alone.
		return "LIMIT 18446744073709551615 OFFSET " + strconv.Itoa(*offset)
	default:
		return "LIMIT " + strconv.Itoa(*limit) + " OFFSET " + strconv.Itoa(*offset)
	}
}

// NullsOrder reproduces PostgreSQL NULL placement with an explicit sort key,
// because MySQL has no NULLS keyword and sorts NULLs first on ASC by default
// (the opposite of PostgreSQL). The key (<col> IS NULL) is 1 for a NULL and 0
// otherwise: ASC on the key puts non-NULLs first (NULLS LAST), DESC puts NULLs
// first (NULLS FIRST). The key is never left to the engine default. The PG
// default is NULLS LAST on ASC, NULLS FIRST on DESC, overridable by the client.
func (Dialect) NullsOrder(col, dir string, desc bool, nullsFirst *bool) (string, string) {
	first := desc
	if nullsFirst != nil {
		first = *nullsFirst
	}
	keyDir := "ASC" // NULLS LAST
	if first {
		keyDir = "DESC" // NULLS FIRST
	}
	return "(" + col + " IS NULL) " + keyDir, col + " " + dir
}

// Returning reports ok=false. MySQL 8 has no RETURNING, so the data plane
// re-selects the written keys inside the same transaction (spec 06). MariaDB
// 10.5+ does support INSERT/DELETE RETURNING; that is a version-gated variant a
// MariaDB-tuned dialect would override, re-verified in the conformance suite
// (spec 22).
func (Dialect) Returning([]string) (string, bool) { return "", false }

// Upsert builds the MySQL upsert. A merge becomes ON DUPLICATE KEY UPDATE col =
// VALUES(col); an ignore becomes a no-op ON DUPLICATE KEY UPDATE col = col over
// the first payload column.
//
// The conflict target is deliberately ignored: ON DUPLICATE KEY fires on any
// unique or primary key, so a specific target cannot be chosen
// (UpsertConflictTarget = false). A request naming on_conflict is rejected by
// the capability gate (spec 04) before it reaches the compiler.
//
// The no-op update is chosen over INSERT IGNORE on purpose. INSERT IGNORE
// downgrades a broad class of errors (not just duplicate keys) to warnings,
// which would silently accept rows a duplicate-key ignore should not. The no-op
// ON DUPLICATE KEY UPDATE suppresses exactly the duplicate-key error and nothing
// else, so an ignore never hides an unrelated failure (the never-silently-wrong
// rule, spec 04/06).
func (Dialect) Upsert(spec sqlgen.UpsertSpec) (string, error) {
	if len(spec.Update) == 0 {
		// An upsert with no payload columns has nothing to update or self-assign;
		// this is a degenerate request the planner does not produce, but guard it
		// rather than emit invalid SQL.
		return "", errEmptyUpsert
	}
	if spec.Ignore {
		c := spec.Update[0]
		return "ON DUPLICATE KEY UPDATE " + c + " = " + c, nil
	}
	parts := make([]string, len(spec.Update))
	for i, c := range spec.Update {
		parts[i] = c + " = VALUES(" + c + ")"
	}
	return "ON DUPLICATE KEY UPDATE " + strings.Join(parts, ", "), nil
}

// errEmptyUpsert is returned when an upsert reaches the dialect with no payload
// columns, which MySQL cannot express.
var errEmptyUpsert = mysqlError("upsert needs at least one column on MySQL")

type mysqlError string

func (e mysqlError) Error() string { return string(e) }

// JSONObject assembles a JSON object with JSON_OBJECT, key order fixed by the
// argument order (the select order).
func (Dialect) JSONObject(pairs []sqlgen.Pair) string {
	parts := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		parts = append(parts, "'"+strings.ReplaceAll(p.Key, "'", "''")+"'", p.Value)
	}
	return "JSON_OBJECT(" + strings.Join(parts, ", ") + ")"
}

// JSONAgg aggregates rows with JSON_ARRAYAGG. MySQL's aggregate takes no ORDER BY
// argument, so a requested embed order is applied on the derived table feeding
// the aggregate, not here; orderBy is therefore unused and the row order within
// the array is best-effort (spec 06, re-verified in spec 22).
func (Dialect) JSONAgg(elem, _ string) string {
	return "JSON_ARRAYAGG(" + elem + ")"
}

// Cast translates a canonical type to one of MySQL's restricted CAST targets.
// MySQL allows only SIGNED/UNSIGNED/CHAR/DECIMAL/DATE/DATETIME/JSON (and a few
// more), with no AS INTEGER or AS TEXT spelling, so the canonical names fold
// onto that set.
func (Dialect) Cast(expr, canonicalType string) string {
	return "CAST(" + expr + " AS " + mysqlType(canonicalType) + ")"
}

// mysqlType maps a canonical type name to a permitted MySQL CAST target.
func mysqlType(canonical string) string {
	switch canonical {
	case "int", "integer", "int2", "int4", "int8", "bigint", "smallint":
		return "SIGNED"
	case "numeric", "decimal", "real", "float", "float4", "float8", "double precision":
		return "DECIMAL"
	case "bool", "boolean":
		// MySQL has no boolean CAST target; coerce through SIGNED (0/1).
		return "SIGNED"
	case "text", "varchar", "char", "uuid":
		return "CHAR"
	case "date":
		return "DATE"
	case "timestamp", "timestamptz":
		return "DATETIME"
	case "json", "jsonb":
		return "JSON"
	default:
		return "CHAR"
	}
}

// Regex renders REGEXP_LIKE(expr, pat). MySQL 8 backs it with ICU; the
// case-insensitive form passes the 'i' match-control argument. The pattern is
// bound, so the fragment carries the PatternMark sentinel where the placeholder
// goes.
func (Dialect) Regex(expr, _ string, ci bool) (string, bool) {
	if ci {
		return "REGEXP_LIKE(" + expr + ", " + sqlgen.PatternMark + ", 'i')", true
	}
	return "REGEXP_LIKE(" + expr + ", " + sqlgen.PatternMark + ")", true
}

// RegexFeatureGap reports no gap. MySQL 8's ICU regex supports backreferences
// and lookaround, so the constructs an RE2-backed engine must reject up front
// compile here; the remaining flavor differences are the documented Best-effort
// surface (the conformance allowlist, spec 22), not flagged before lowering.
func (Dialect) RegexFeatureGap(string) string { return "" }

// SessionRead reports no engine-side setting store. MySQL has no GUC-in-SQL, so
// a request-context value a policy references is bound as a parameter when the
// predicate is injected (spec 15), not read from the engine. The empty form
// tells the compiler there is nothing to read.
func (Dialect) SessionRead(string) string { return "" }

// SessionWrite reports ok=false: there is no engine setting to write.
func (Dialect) SessionWrite(string) (string, bool) { return "", false }

// BoolValue renders a boolean as 1/0. MySQL's BOOL is an alias for TINYINT(1),
// so there is no native boolean keyword.
func (Dialect) BoolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// ILike uses plain LIKE; MySQL's default collation is case-insensitive.
func (Dialect) ILike(col, val string) (string, bool) { return col + " LIKE " + val, true }
