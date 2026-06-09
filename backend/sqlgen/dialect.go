// Package sqlgen holds the single IR-to-SQL compiler shared by every SQL
// backend. The compiler walks a resolved plan (spec 05) and emits a
// parameterized statement plus its argument list; wherever the SQL is
// engine-specific it asks the active Dialect for the fragment instead of
// branching on the engine. See spec 06-relational-dialects.
//
// Two invariants hold by construction: every value becomes a bound parameter
// (never interpolated), and every identifier passes through Dialect.QuoteIdent.
// A name that is not in the schema model never reaches the compiler; planning
// rejects it first.
package sqlgen

import "github.com/tamnd/dbrest/ir"

// Dialect is the per-engine spelling of the fragments that differ between SQL
// engines. One Dialect plus one Capabilities is the whole of what a new SQL
// engine must supply; the compiler, planner, and renderer are reused. See the
// per-engine profiles in spec 06.
type Dialect interface {
	// QuoteIdent quotes an identifier for safe embedding, doubling the engine's
	// quote character.
	QuoteIdent(name string) string

	// Placeholder renders the n-th bind placeholder (1-based). The compiler
	// assigns positions as it appends arguments.
	Placeholder(n int) string

	// LimitOffset emits the pagination clause. hasOrder reports whether the
	// query already carries an ORDER BY, for engines that require one.
	LimitOffset(limit, offset *int, hasOrder bool) string

	// NullsOrder places NULLs in an ORDER BY term to match PostgreSQL. It returns
	// an optional synthetic sort-key expression (prepended to the ORDER BY) and
	// the column's own order term. col is already quoted.
	NullsOrder(col, dir string, desc bool, nullsFirst *bool) (sortKey, orderTerm string)

	// Returning emits a clause returning the written rows, or reports ok=false so
	// the compiler re-selects by key. cols are already quoted.
	Returning(cols []string) (clause string, ok bool)

	// Upsert builds the upsert clause for merge/ignore duplicates.
	Upsert(spec UpsertSpec) (string, error)

	// JSONObject assembles a JSON object from key/value pairs, in the engine.
	JSONObject(pairs []Pair) string

	// JSONAgg aggregates rows into a JSON array, with optional ordering.
	JSONAgg(elem, orderBy string) string

	// Cast translates a ::canonicalType cast to the engine's spelling.
	Cast(expr, canonicalType string) string

	// Regex renders match/imatch against a pattern, or reports ok=false when the
	// engine has no regex so the planner marks the feature Unsupported.
	Regex(expr, pattern string, ci bool) (string, bool)

	// RegexFeatureGap reports a regex construct in pattern that the engine's regex
	// flavor cannot honor and dbrest detects before lowering, naming it for a
	// PGRST127; it returns "" when the pattern is within the engine's grammar. See
	// spec 21, "the semantic-divergence problem".
	RegexFeatureGap(pattern string) string

	// FullText lowers a full-text predicate. col is the quoted column reference
	// (PostgreSQL builds to_tsvector over it); idx is the resolved covering index,
	// or nil when the schema has none (an engine that requires one reports ok=false
	// and the compiler raises PGRST127). variant is the fts/plfts/phfts/wfts
	// grammar; config is the language argument (may be empty); value is the raw
	// query text. The returned fragment carries PatternMark where the bound,
	// engine-translated query value goes, and bind is that value. See spec 21.
	FullText(col string, idx *FullTextRef, variant ir.FTSVariant, config, value string) (frag, bind string, ok bool)

	// SessionRead reads a request-context value (the GUC analog).
	SessionRead(key string) string

	// SessionWrite writes a request-context value, or reports ok=false when the
	// engine has no SQL-readable session store.
	SessionWrite(key string) (stmt string, ok bool)

	// ILike renders a case-insensitive LIKE predicate, or reports ok=false when
	// the engine does not support it (compiler emits PGRST127). col and val are
	// already-compiled SQL fragments. Engines where plain LIKE is case-insensitive
	// by default (MySQL, SQLite, SQL Server typical collation) return ok=true with
	// a LIKE expression; PostgreSQL returns ILIKE.
	ILike(col, val string) (frag string, ok bool)

	// BoolValue renders a boolean literal.
	BoolValue(v bool) string

	// ArrayOp renders an array containment/overlap operator expression, or
	// reports ok=false when the engine does not support array types (SQLite,
	// MySQL, SQL Server). The compiler emits PGRST127 when ok=false.
	// op is one of "@>", "<@", "&&"; col is the quoted column; val is the
	// placeholder returned by bind().
	ArrayOp(col, op, val string) (string, bool)
}

// PatternMark is the sentinel a Dialect.Regex fragment carries where the bound
// pattern placeholder belongs. The compiler substitutes the real placeholder for
// it, so a dialect that itself needs a literal ? (such as a (?i) prefix) is not
// disturbed.
const PatternMark = "$PAT$"

// FullTextRef is the resolved engine-side structure a full-text predicate lowers
// against, built by the compiler from the schema's covering index. Table is the
// quoted full-text object (the FTS5 virtual table); RowidRef is the quoted
// base-table rowid reference that joins it back to the row.
type FullTextRef struct {
	Table    string
	RowidRef string
}

// Pair is a key/value entry for JSONObject. Key is a literal JSON key; Value is
// an already-compiled SQL expression.
type Pair struct {
	Key   string
	Value string
}

// UpsertSpec carries the conflict target and update set for Dialect.Upsert.
type UpsertSpec struct {
	// Target is the conflict-target columns (already quoted), or empty for the
	// engine's any-unique-key behavior.
	Target []string
	// Update is the set of columns to update on conflict (already quoted).
	Update []string
	// Ignore selects ignore-duplicates (DO NOTHING) over merge-duplicates.
	Ignore bool
}
