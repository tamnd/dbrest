package sqlite

import (
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// FullText lowers an fts predicate to an FTS5 MATCH. SQLite has no full-text on
// ordinary tables, so the match runs against the FTS5 virtual table that shadows
// the column and is joined back by rowid:
//
//	<rowid> IN (SELECT rowid FROM <fts> WHERE <fts> MATCH ?)
//
// With no covering FTS5 table the predicate is unavailable (ok=false) and the
// compiler raises PGRST127; SQLite never silently scans. The config (language)
// argument is ignored because an FTS5 tokenizer is fixed at table-create time
// (spec 21); the divergence is documented, not an error. The bound value is the
// query text translated to FTS5 query syntax for the variant. The col argument is
// unused: the join goes through the index's rowid, not the base column directly.
func (dialect) FullText(_, _ string, idx *sqlgen.FullTextRef, variant ir.FTSVariant, _, value string) (string, string, bool) {
	if idx == nil {
		return "", "", false
	}
	frag := idx.RowidRef + " IN (SELECT rowid FROM " + idx.Table +
		" WHERE " + idx.Table + " MATCH " + sqlgen.PatternMark + ")"
	return frag, fts5Query(variant, value), true
}

// fts5Query translates a PostgREST full-text value into an FTS5 query string for
// the variant. FTS5 has its own boolean grammar (AND/OR/NOT, "phrases"), so each
// variant is mapped as spec 21 prescribes; ordinary terms are quoted so a word is
// never read as an FTS5 operator or bareword keyword. This is Best-effort: FTS5
// tokenization and ranking are not PostgreSQL's.
func fts5Query(v ir.FTSVariant, value string) string {
	switch v {
	case ir.FTSPhrase:
		// phfts keeps word order: the whole value is one quoted phrase.
		return fts5Quote(strings.Join(strings.Fields(value), " "))
	case ir.FTSPlainText:
		// plfts ANDs the lexemes.
		return fts5AndTerms(value)
	case ir.FTSWeb:
		// wfts is a web-style string: quoted phrases, `or`, and `-` for NOT.
		return fts5Web(value)
	default:
		// fts is to_tsquery grammar: &, |, !, grouping, phrase adjacency.
		return fts5Boolean(value)
	}
}

// fts5Quote wraps a term as an FTS5 string literal, doubling embedded quotes so a
// term with punctuation is matched verbatim rather than parsed as syntax.
func fts5Quote(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

// fts5AndTerms quotes each whitespace-separated term and joins them with AND, the
// plfts (plainto_tsquery) semantics.
func fts5AndTerms(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return `""`
	}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fts5Quote(f)
	}
	return strings.Join(parts, " AND ")
}

// fts5Boolean translates to_tsquery operators into FTS5 boolean syntax: & is AND,
// | is OR, ! is NOT, parentheses group, and the phrase-adjacency operator <-> is
// approximated as AND (FTS5's NEAR is a function form, not an infix operator).
// Word lexemes are quoted. This is the fts variant, Best-effort by nature.
func fts5Boolean(value string) string {
	var out []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			out = append(out, fts5Quote(word.String()))
			word.Reset()
		}
	}
	for i := 0; i < len(value); {
		switch c := value[i]; {
		case strings.HasPrefix(value[i:], "<->"):
			flush()
			out = append(out, "AND")
			i += 3
		case c == '&':
			flush()
			out = append(out, "AND")
			i++
		case c == '|':
			flush()
			out = append(out, "OR")
			i++
		case c == '!':
			flush()
			out = append(out, "NOT")
			i++
		case c == '(' || c == ')':
			flush()
			out = append(out, string(c))
			i++
		case c == ' ' || c == '\t':
			flush()
			i++
		default:
			word.WriteByte(c)
			i++
		}
	}
	flush()
	return strings.Join(out, " ")
}

// fts5Web translates a websearch-style string into FTS5: a "quoted phrase" stays
// a phrase, a bare `or` becomes OR, a `-term` becomes NOT term, and consecutive
// terms keep FTS5's implicit AND. This is the wfts variant.
func fts5Web(value string) string {
	var out []string
	for _, tok := range splitWeb(value) {
		switch {
		case tok == "":
		case strings.EqualFold(tok, "or"):
			out = append(out, "OR")
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			out = append(out, "NOT", fts5Quote(unquoteWeb(tok[1:])))
		default:
			out = append(out, fts5Quote(unquoteWeb(tok)))
		}
	}
	return strings.Join(out, " ")
}

// splitWeb splits a web-style query on whitespace while keeping a "quoted phrase"
// as a single token (its surrounding quotes retained for unquoteWeb to strip).
func splitWeb(value string) []string {
	var out []string
	var tok strings.Builder
	inQuote := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			tok.WriteByte(c)
		case (c == ' ' || c == '\t') && !inQuote:
			if tok.Len() > 0 {
				out = append(out, tok.String())
				tok.Reset()
			}
		default:
			tok.WriteByte(c)
		}
	}
	if tok.Len() > 0 {
		out = append(out, tok.String())
	}
	return out
}

// unquoteWeb strips the surrounding double quotes from a web phrase token, leaving
// an ordinary term untouched.
func unquoteWeb(tok string) string {
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		return tok[1 : len(tok)-1]
	}
	return strings.Trim(tok, `"`)
}

// RegexFeatureGap detects regex constructs the RE2 engine cannot honor, so a
// pattern that would otherwise mean something else (or error deep in the engine)
// is rejected up front with a named feature. RE2, which backs the registered
// regexp(), has no backreferences and no lookaround; both are detectable
// lexically. Other POSIX/RE2 edge differences compile on RE2 but match a slightly
// different set; they are the documented Best-effort surface (the conformance
// allowlist, spec 22), not flagged here. See spec 21.
func (dialect) RegexFeatureGap(pattern string) string {
	for i := 0; i < len(pattern); i++ {
		switch {
		case pattern[i] == '\\' && i+1 < len(pattern):
			if d := pattern[i+1]; d >= '1' && d <= '9' {
				return "regex backreferences (the RE2 engine has none)"
			}
			i++ // skip the escaped character
		case strings.HasPrefix(pattern[i:], "(?="),
			strings.HasPrefix(pattern[i:], "(?!"),
			strings.HasPrefix(pattern[i:], "(?<="),
			strings.HasPrefix(pattern[i:], "(?<!"):
			return "regex lookaround (the RE2 engine has none)"
		}
	}
	return ""
}
