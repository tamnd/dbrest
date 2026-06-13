package sqlserver

import (
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// FullText lowers a full-text predicate to a SQL Server CONTAINS or FREETEXT
// predicate (spec 21). SQL Server full text needs a full-text catalog and a
// full-text index on the column; the planner detects a missing index during
// introspection and raises PGRST127, so dbrest never silently scans. The query
// value carries the variant's grammar translated into the predicate's search
// syntax and is bound (the fragment carries PatternMark where the placeholder
// goes). The translation is Best-effort: SQL Server's word breakers and stemmers
// differ from PostgreSQL dictionaries, and CONTAINS boolean syntax is not a
// one-to-one image of to_tsquery, so the divergence is documented in the
// conformance allowlist (spec 22).
//
//   - plfts (plainto_tsquery, meaning-based) maps to FREETEXT, which handles
//     inflection the way plainto_tsquery's dictionary normalization does.
//   - the other variants map to CONTAINS, whose AND / OR / AND NOT / NEAR
//     operators give the explicit set semantics to_tsquery has.
func (Dialect) FullText(col, _ string, _ *sqlgen.FullTextRef, variant ir.FTSVariant, _, value string) (string, string, bool) {
	if variant == ir.FTSPlainText {
		// FREETEXT takes a natural-language string, no operators; collapse runs of
		// whitespace so the bound value is clean.
		return "FREETEXT(" + col + ", " + sqlgen.PatternMark + ")", strings.Join(strings.Fields(value), " "), true
	}
	return "CONTAINS(" + col + ", " + sqlgen.PatternMark + ")", containsValue(variant, value), true
}

// containsValue builds the CONTAINS search condition for a non-FREETEXT variant.
func containsValue(v ir.FTSVariant, value string) string {
	switch v {
	case ir.FTSPhrase:
		// phfts keeps word order: the whole value is one quoted phrase.
		return containsPhrase(strings.Join(strings.Fields(value), " "))
	case ir.FTSWeb:
		// wfts is a web-style string: "phrases", a bare or, and -term to exclude.
		return webContains(value)
	default:
		// fts is to_tsquery grammar (&, |, !, <->); map each operator onto its
		// CONTAINS spelling.
		return containsQuery(value)
	}
}

// containsPhrase wraps a value as a CONTAINS phrase literal, dropping embedded
// double quotes so they are not read as phrase delimiters.
func containsPhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, "") + `"`
}

// fts token kinds for the to_tsquery-to-CONTAINS translation.
const (
	tkTerm = iota
	tkAnd  // &
	tkOr   // |
	tkNot  // !
	tkNear // <->
	tkLParen
	tkRParen
)

type ftsToken struct {
	kind int
	text string // set for tkTerm
}

// containsQuery translates to_tsquery operators into a CONTAINS search
// condition. Unlike MySQL boolean mode, CONTAINS has real AND / OR / AND NOT /
// NEAR operators, so the translation is a straight token-by-token mapping: each
// term becomes a "quoted" term, & becomes AND, | becomes OR, ! becomes NOT
// (combining with a preceding & to read as AND NOT), the phrase-adjacency <->
// becomes NEAR, and parentheses pass through for grouping. This is the fts
// variant, Best-effort (spec 22).
func containsQuery(value string) string {
	toks := tokenizeFTS(value)
	var out []string
	for _, t := range toks {
		switch t.kind {
		case tkTerm:
			out = append(out, containsTerm(t.text))
		case tkAnd:
			out = append(out, "AND")
		case tkOr:
			out = append(out, "OR")
		case tkNot:
			out = append(out, "NOT")
		case tkNear:
			out = append(out, "NEAR")
		case tkLParen:
			out = append(out, "(")
		case tkRParen:
			out = append(out, ")")
		}
	}
	return strings.Join(out, " ")
}

// tokenizeFTS splits a to_tsquery string into terms and operators.
func tokenizeFTS(value string) []ftsToken {
	var toks []ftsToken
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			toks = append(toks, ftsToken{kind: tkTerm, text: word.String()})
			word.Reset()
		}
	}
	for i := 0; i < len(value); {
		switch c := value[i]; {
		case strings.HasPrefix(value[i:], "<->"):
			flush()
			toks = append(toks, ftsToken{kind: tkNear})
			i += 3
		case c == '&':
			flush()
			toks = append(toks, ftsToken{kind: tkAnd})
			i++
		case c == '|':
			flush()
			toks = append(toks, ftsToken{kind: tkOr})
			i++
		case c == '!':
			flush()
			toks = append(toks, ftsToken{kind: tkNot})
			i++
		case c == '(':
			flush()
			toks = append(toks, ftsToken{kind: tkLParen})
			i++
		case c == ')':
			flush()
			toks = append(toks, ftsToken{kind: tkRParen})
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
	return toks
}

// webContains translates a websearch-style string into a CONTAINS search
// condition: a "quoted phrase" stays a phrase, a bare or joins its neighbors
// with OR, a -term excludes (AND NOT), and an ordinary term is required (joined
// with AND). This is the wfts variant, Best-effort.
func webContains(value string) string {
	toks := splitWeb(value)
	var out []string
	conn := "" // connector to apply before the next term; OR after a bare "or"
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch {
		case tok == "":
		case strings.EqualFold(tok, "or"):
			conn = "OR"
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			out = appendTerm(out, conn, "NOT", webTerm(tok[1:]))
			conn = ""
		default:
			out = appendTerm(out, conn, "", webTerm(tok))
			conn = ""
		}
	}
	return strings.Join(out, " ")
}

// appendTerm joins a term onto the output with the right CONTAINS connector. The
// first term takes no leading connector (a leading exclusion degrades to a bare
// NOT, which is the best CONTAINS can do for that ill-formed input); a later term
// takes base (AND unless a bare "or" set OR) optionally suffixed with NOT for an
// exclusion.
func appendTerm(out []string, base, not, term string) []string {
	if len(out) == 0 {
		if not != "" {
			return append(out, not, term)
		}
		return append(out, term)
	}
	conn := base
	if conn == "" {
		conn = "AND"
	}
	if not != "" {
		conn += " " + not
	}
	return append(out, conn, term)
}

// webTerm renders a web token as a CONTAINS phrase or single quoted term.
func webTerm(tok string) string {
	t := unquoteWeb(tok)
	return containsPhrase(t)
}

// containsTerm quotes a single term for a CONTAINS condition, dropping embedded
// double quotes so a term carrying one is not read as a phrase delimiter.
func containsTerm(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, "") + `"`
}

// splitWeb splits a web-style query on whitespace while keeping a "quoted
// phrase" as one token (its surrounding quotes retained for unquoteWeb).
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

// unquoteWeb strips surrounding double quotes from a web phrase token, leaving an
// ordinary term untouched.
func unquoteWeb(tok string) string {
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		return tok[1 : len(tok)-1]
	}
	return strings.Trim(tok, `"`)
}
