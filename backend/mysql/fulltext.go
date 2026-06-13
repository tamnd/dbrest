package mysql

import (
	"strings"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// FullText lowers an fts predicate to a MySQL MATCH ... AGAINST in boolean mode:
//
//	MATCH(col) AGAINST(? IN BOOLEAN MODE)
//
// MySQL full text runs against a FULLTEXT index on the base column itself, so
// the match uses col directly and idx is unused (unlike SQLite's separate FTS5
// virtual table). When the column has no FULLTEXT index MySQL errors at runtime;
// the planner detects that during introspection and raises PGRST127, so dbrest
// never silently scans. Boolean mode is used for every variant so the query
// semantics are explicit operators rather than relevance ranking, which keeps
// the result a set membership test like to_tsquery rather than a ranked search.
//
// The query value carries the variant's grammar translated into boolean-mode
// syntax, and is bound (the fragment carries PatternMark where the placeholder
// goes). The translation is Best-effort: MySQL boolean mode has no general
// AND/OR/grouping the way to_tsquery does, so disjunction and grouping are
// approximated and documented in the conformance allowlist (spec 22).
func (Dialect) FullText(col, _ string, _ *sqlgen.FullTextRef, variant ir.FTSVariant, _, value string) (string, string, bool) {
	frag := "MATCH(" + col + ") AGAINST(" + sqlgen.PatternMark + " IN BOOLEAN MODE)"
	return frag, booleanModeQuery(variant, value), true
}

// booleanModeQuery translates a PostgREST full-text value into a MySQL
// boolean-mode search string for the variant.
func booleanModeQuery(v ir.FTSVariant, value string) string {
	switch v {
	case ir.FTSPhrase:
		// phfts keeps word order: the whole value is one quoted phrase.
		return quotePhrase(strings.Join(strings.Fields(value), " "))
	case ir.FTSPlainText:
		// plfts requires every lexeme: each term is prefixed +.
		return requiredTerms(value)
	case ir.FTSWeb:
		// wfts is a web-style string: "phrases", a bare or for optional terms, and
		// -term to exclude.
		return webQuery(value)
	default:
		// fts is to_tsquery grammar (&, |, !, grouping); map it onto boolean mode.
		return booleanQuery(value)
	}
}

// quotePhrase wraps a value as a boolean-mode phrase literal, escaping embedded
// double quotes so punctuation is matched verbatim rather than read as syntax.
func quotePhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, ``) + `"`
}

// requiredTerms prefixes each whitespace-separated term with +, the plfts
// (plainto_tsquery) all-terms-required semantics.
func requiredTerms(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = "+" + boolTerm(f)
	}
	return strings.Join(parts, " ")
}

// fts token kinds for the to_tsquery-to-boolean-mode translation.
const (
	tkTerm = iota
	tkAnd  // & or <->
	tkOr   // |
	tkNot  // !
	tkLParen
	tkRParen
)

type ftsToken struct {
	kind int
	text string // set for tkTerm
}

// booleanQuery translates to_tsquery operators into MySQL boolean mode. It
// tokenizes the value, then assigns each term a prefix from its neighboring
// operators: a term next to | is left optional (boolean mode has no OR, so an
// optional term widens the match the way OR would); a term after ! is excluded
// (-); every other term is required (+). & and the phrase-adjacency <-> both
// mean required-on-both-sides, which the required default already gives.
// Parentheses pass through for grouping. This is the fts variant, Best-effort
// (spec 22), since boolean mode cannot reproduce arbitrary OR/grouping exactly.
func booleanQuery(value string) string {
	toks := tokenizeFTS(value)
	var out []string
	for i, t := range toks {
		switch t.kind {
		case tkLParen:
			out = append(out, "(")
		case tkRParen:
			out = append(out, ")")
		case tkTerm:
			out = append(out, ftsPrefix(toks, i)+boolTerm(t.text))
		}
	}
	return strings.Join(out, " ")
}

// ftsPrefix decides a term's boolean-mode prefix from its neighbors: excluded if
// the previous operator is !, optional if either adjacent operator is |, and
// required otherwise.
func ftsPrefix(toks []ftsToken, i int) string {
	prev, next := prevOp(toks, i), nextOp(toks, i)
	if prev == tkNot {
		return "-"
	}
	if prev == tkOr || next == tkOr {
		return ""
	}
	return "+"
}

// prevOp returns the kind of the operator immediately before term i, or -1 when
// the nearest preceding token is not an operator (a paren or the start).
func prevOp(toks []ftsToken, i int) int {
	if i == 0 {
		return -1
	}
	switch toks[i-1].kind {
	case tkAnd, tkOr, tkNot:
		return toks[i-1].kind
	default:
		return -1
	}
}

// nextOp returns the kind of the operator immediately after term i, or -1.
func nextOp(toks []ftsToken, i int) int {
	if i+1 >= len(toks) {
		return -1
	}
	switch toks[i+1].kind {
	case tkAnd, tkOr, tkNot:
		return toks[i+1].kind
	default:
		return -1
	}
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
			toks = append(toks, ftsToken{kind: tkAnd})
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

// webQuery translates a websearch-style string into boolean mode: a "quoted
// phrase" stays a phrase, a bare or makes the surrounding terms optional, a
// -term excludes, and an ordinary term is required (+). This is the wfts
// variant, Best-effort.
func webQuery(value string) string {
	toks := splitWeb(value)
	var out []string
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch {
		case tok == "":
		case strings.EqualFold(tok, "or"):
			// or relaxes the adjacent required terms to optional: drop the + from the
			// previous term and skip prefixing the next.
			if len(out) > 0 {
				out[len(out)-1] = strings.TrimPrefix(out[len(out)-1], "+")
			}
			if i+1 < len(toks) {
				out = append(out, boolWebTerm(toks[i+1], false))
				i++
			}
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			out = append(out, "-"+boolWebTerm(tok[1:], false))
		default:
			out = append(out, boolWebTerm(tok, true))
		}
	}
	return strings.Join(out, " ")
}

// boolWebTerm renders a web token as a phrase or a plain term, optionally
// prefixed + to make it required.
func boolWebTerm(tok string, required bool) string {
	t := unquoteWeb(tok)
	var rendered string
	if tok != t || strings.ContainsAny(t, " \t") {
		rendered = quotePhrase(t)
	} else {
		rendered = boolTerm(t)
	}
	if required {
		return "+" + rendered
	}
	return rendered
}

// boolTerm sanitizes a single term so a boolean-mode operator character embedded
// in a word is not read as syntax; a term with such a character is quoted as a
// one-word phrase.
func boolTerm(term string) string {
	if strings.ContainsAny(term, `+-<>()~*"@ `) {
		return quotePhrase(term)
	}
	return term
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
