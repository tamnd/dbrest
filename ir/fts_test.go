package ir

import "testing"

// fetchCompare pulls the single Compare node a one-filter query string parses to.
func fetchCompare(t *testing.T, query string) Compare {
	t.Helper()
	q, err := ParseRead("films", query, nil)
	if err != nil {
		t.Fatalf("ParseRead(%q): %v", query, err)
	}
	if q.Where == nil {
		t.Fatalf("ParseRead(%q): no filter parsed", query)
	}
	cmp, ok := (*q.Where).(Compare)
	if !ok {
		t.Fatalf("ParseRead(%q): filter is %T, want Compare", query, *q.Where)
	}
	return cmp
}

func TestParseFTSVariants(t *testing.T) {
	cases := []struct {
		query   string
		variant FTSVariant
		value   string
	}{
		{"body=fts.cat", FTSPlain, "cat"},
		{"body=plfts.the cat", FTSPlainText, "the cat"},
		{"body=phfts.the cat", FTSPhrase, "the cat"},
		{"body=wfts.the cat", FTSWeb, "the cat"},
	}
	for _, c := range cases {
		cmp := fetchCompare(t, c.query)
		if cmp.Op != OpFTS {
			t.Errorf("%q: Op = %v, want OpFTS", c.query, cmp.Op)
		}
		if cmp.FTS != c.variant {
			t.Errorf("%q: FTS = %d, want %d", c.query, cmp.FTS, c.variant)
		}
		if cmp.Value.Text != c.value {
			t.Errorf("%q: value = %q, want %q", c.query, cmp.Value.Text, c.value)
		}
		if cmp.Config != "" {
			t.Errorf("%q: Config = %q, want empty", c.query, cmp.Config)
		}
	}
}

// TestParseFTSConfig is the regression for the parser reading fts(english) as a
// quantifier and erroring "unknown quantifier: english". The parenthesized
// argument on a full-text operator is the language config, not a quantifier.
func TestParseFTSConfig(t *testing.T) {
	cmp := fetchCompare(t, "body=fts(english).cat")
	if cmp.Op != OpFTS || cmp.FTS != FTSPlain {
		t.Fatalf("Op/FTS = %v/%d, want OpFTS/FTSPlain", cmp.Op, cmp.FTS)
	}
	if cmp.Config != "english" {
		t.Errorf("Config = %q, want english", cmp.Config)
	}
	if cmp.Value.Text != "cat" {
		t.Errorf("value = %q, want cat", cmp.Value.Text)
	}
}

func TestParseFTSConfigOnEveryVariant(t *testing.T) {
	for _, q := range []string{
		"body=fts(german).hund", "body=plfts(german).hund",
		"body=phfts(german).hund", "body=wfts(german).hund",
	} {
		cmp := fetchCompare(t, q)
		if cmp.Op != OpFTS {
			t.Errorf("%q: Op = %v, want OpFTS", q, cmp.Op)
		}
		if cmp.Config != "german" {
			t.Errorf("%q: Config = %q, want german", q, cmp.Config)
		}
	}
}

// TestParseFTSNegated checks not.fts keeps the full-text op and its value.
func TestParseFTSNegated(t *testing.T) {
	cmp := fetchCompare(t, "body=not.fts(english).cat")
	if !cmp.Negate {
		t.Error("Negate = false, want true")
	}
	if cmp.Op != OpFTS || cmp.Config != "english" || cmp.Value.Text != "cat" {
		t.Errorf("got Op=%v Config=%q value=%q", cmp.Op, cmp.Config, cmp.Value.Text)
	}
}

// TestQuantifierStillParses guards that splitting fts-config from the quantifier
// branch did not break op(any)/op(all) on the comparison operators.
func TestQuantifierStillParses(t *testing.T) {
	cmp := fetchCompare(t, "id=eq(any).1")
	if cmp.Op != OpEq {
		t.Errorf("Op = %v, want OpEq", cmp.Op)
	}
	if cmp.Quant != QAny {
		t.Errorf("Quant = %d, want QAny", cmp.Quant)
	}
}

func TestUnknownQuantifierStillErrors(t *testing.T) {
	if _, err := ParseRead("films", "id=eq(nonsense).1", nil); err == nil {
		t.Fatal("eq(nonsense) should be a parse error")
	}
}
