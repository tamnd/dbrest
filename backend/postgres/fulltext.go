package postgres

import (
	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// FullText lowers an fts predicate to PostgreSQL's native tsvector match:
//
//	to_tsvector(<config>, col) @@ <ctor>(<config>, <value>)
//
// PostgreSQL builds the search vector over any text column on the fly, so there
// is no covering-index requirement and idx is unused (a nil idx is fine, unlike
// SQLite's FTS5). The query constructor follows the variant: to_tsquery for the
// boolean grammar, plainto_tsquery for ANDed lexemes, phraseto_tsquery for an
// ordered phrase, websearch_to_tsquery for a web-style string. The value carries
// the variant's own grammar, which PostgreSQL parses itself, so it rides through
// as the bound operand (PatternMark) verbatim rather than being pre-translated
// the way the FTS5 dialect must translate it.
//
// config is the optional language (regconfig). When present it is embedded as an
// escaped string literal in both the vector and the query constructor so the two
// sides tokenize identically; it is one of dbrest's validated configuration
// names, not raw client input, and the Dialect interface carries a bound operand
// only for the query value. An empty config omits the argument, letting the
// server's default_text_search_config apply, which is the PostgREST default.
func (Dialect) FullText(col string, _ *sqlgen.FullTextRef, variant ir.FTSVariant, config, value string) (string, string, bool) {
	ctor := map[ir.FTSVariant]string{
		ir.FTSPlain:     "to_tsquery",
		ir.FTSPlainText: "plainto_tsquery",
		ir.FTSPhrase:    "phraseto_tsquery",
		ir.FTSWeb:       "websearch_to_tsquery",
	}[variant]

	var cfg string
	if config != "" {
		cfg = sqlLiteral(config) + ", "
	}
	frag := "to_tsvector(" + cfg + col + ") @@ " + ctor + "(" + cfg + sqlgen.PatternMark + ")"
	// The value carries the variant's grammar (boolean operators, quoted phrases,
	// a web-style string), which PostgreSQL parses itself, so it is bound verbatim
	// rather than pre-translated the way the FTS5 dialect must translate it.
	return frag, value, true
}
