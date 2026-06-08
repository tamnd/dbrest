package mysql

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// fts returns the boolean-mode query a variant lowers a value to. The MATCH
// wrapper is fixed and snapshotted in compile_test; these cases pin the grammar
// translation, the part that carries the per-variant divergence.
func fts(v ir.FTSVariant, value string) string {
	_, q, _ := Dialect{}.FullText("`c`", nil, v, "", value)
	return q
}

func TestFullTextWrapper(t *testing.T) {
	frag, _, ok := Dialect{}.FullText("`c`", nil, ir.FTSPlain, "", "x")
	if !ok || frag != "MATCH(`c`) AGAINST($PAT$ IN BOOLEAN MODE)" {
		t.Errorf("frag = %q, ok = %v", frag, ok)
	}
}

func TestPhraseVariant(t *testing.T) {
	// phfts keeps order, as one quoted phrase.
	if got := fts(ir.FTSPhrase, "quick  brown fox"); got != `"quick brown fox"` {
		t.Errorf("phrase = %q", got)
	}
}

func TestPlainTextVariant(t *testing.T) {
	// plfts requires every term.
	if got := fts(ir.FTSPlainText, "quick brown"); got != "+quick +brown" {
		t.Errorf("plain = %q", got)
	}
}

func TestFtsVariant(t *testing.T) {
	cases := map[string]string{
		// & is required on both sides.
		"quick & brown": "+quick +brown",
		// | makes both operands optional (boolean mode has no OR).
		"quick | brown": "quick brown",
		// ! excludes the following term.
		"quick & !slow": "+quick -slow",
		// the phrase-adjacency operator behaves like &.
		"quick <-> brown": "+quick +brown",
		// a three-way & chain stays all-required.
		"a & b & c": "+a +b +c",
		// grouping parens pass through.
		"a & (b | c)": "+a ( b c )",
	}
	for in, want := range cases {
		if got := fts(ir.FTSPlain, in); got != want {
			t.Errorf("fts(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebVariant(t *testing.T) {
	cases := map[string]string{
		// bare terms are required.
		"cat dog": "+cat +dog",
		// a quoted phrase stays a phrase, required like any other bare term.
		`"cat dog" bird`: `+"cat dog" +bird`,
		// -term excludes.
		"cat -dog": "+cat -dog",
		// a bare or relaxes the terms it joins to optional.
		"cat or dog": "cat dog",
	}
	for in, want := range cases {
		if got := fts(ir.FTSWeb, in); got != want {
			t.Errorf("web(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTermWithOperatorChar checks that a boolean-mode operator character inside a
// word is quoted so it is read as text, not syntax.
func TestTermWithOperatorChar(t *testing.T) {
	if got := fts(ir.FTSPlainText, "c++"); got != `+"c++"` {
		t.Errorf("operator-char term = %q", got)
	}
}
