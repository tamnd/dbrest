package sqlserver

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// ftsFrag and fts return the predicate wrapper and the translated query value a
// variant lowers to.
func ftsFrag(v ir.FTSVariant) string {
	frag, _, _ := Dialect{}.FullText("[c]", "", nil, v, "", "x")
	return frag
}

func fts(v ir.FTSVariant, value string) string {
	_, q, _ := Dialect{}.FullText("[c]", "", nil, v, "", value)
	return q
}

func TestFullTextPredicates(t *testing.T) {
	// plfts maps to FREETEXT; every other variant maps to CONTAINS.
	if got := ftsFrag(ir.FTSPlainText); got != "FREETEXT([c], $PAT$)" {
		t.Errorf("plfts frag = %q", got)
	}
	for _, v := range []ir.FTSVariant{ir.FTSPlain, ir.FTSPhrase, ir.FTSWeb} {
		if got := ftsFrag(v); got != "CONTAINS([c], $PAT$)" {
			t.Errorf("variant %d frag = %q", v, got)
		}
	}
}

func TestFreeTextValue(t *testing.T) {
	// FREETEXT takes a clean natural-language string.
	if got := fts(ir.FTSPlainText, "the  quick fox"); got != "the quick fox" {
		t.Errorf("plfts value = %q", got)
	}
}

func TestPhraseValue(t *testing.T) {
	if got := fts(ir.FTSPhrase, "quick  brown fox"); got != `"quick brown fox"` {
		t.Errorf("phrase value = %q", got)
	}
}

func TestFtsValue(t *testing.T) {
	cases := map[string]string{
		// & becomes AND, | becomes OR.
		"quick & brown": `"quick" AND "brown"`,
		"quick | brown": `"quick" OR "brown"`,
		// ! after & reads as AND NOT.
		"quick & !slow": `"quick" AND NOT "slow"`,
		// the phrase-adjacency operator becomes NEAR.
		"quick <-> brown": `"quick" NEAR "brown"`,
		// grouping passes through.
		"a & (b | c)": `"a" AND ( "b" OR "c" )`,
	}
	for in, want := range cases {
		if got := fts(ir.FTSPlain, in); got != want {
			t.Errorf("fts(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebValue(t *testing.T) {
	cases := map[string]string{
		// bare terms are ANDed.
		"cat dog": `"cat" AND "dog"`,
		// a quoted phrase stays a phrase.
		`"cat dog" bird`: `"cat dog" AND "bird"`,
		// -term excludes with AND NOT.
		"cat -dog": `"cat" AND NOT "dog"`,
		// a bare or joins its neighbors with OR.
		"cat or dog": `"cat" OR "dog"`,
	}
	for in, want := range cases {
		if got := fts(ir.FTSWeb, in); got != want {
			t.Errorf("web(%q) = %q, want %q", in, got, want)
		}
	}
}
