package sqlite

import (
	"context"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
)

func TestFTS5Query(t *testing.T) {
	cases := []struct {
		variant ir.FTSVariant
		value   string
		want    string
	}{
		{ir.FTSPlain, "cat & hat", `"cat" AND "hat"`},
		{ir.FTSPlain, "cat | dog", `"cat" OR "dog"`},
		{ir.FTSPlain, "!cat", `NOT "cat"`},
		{ir.FTSPlain, "cat <-> hat", `"cat" AND "hat"`},
		{ir.FTSPlain, "(cat | dog) & hat", `( "cat" OR "dog" ) AND "hat"`},
		{ir.FTSPlainText, "the cat sat", `"the" AND "cat" AND "sat"`},
		{ir.FTSPhrase, "the cat sat", `"the cat sat"`},
		{ir.FTSWeb, "cat -dog", `"cat" NOT "dog"`},
		{ir.FTSWeb, `"the cat" or dog`, `"the cat" OR "dog"`},
	}
	for _, c := range cases {
		if got := fts5Query(c.variant, c.value); got != c.want {
			t.Errorf("fts5Query(%d, %q) = %q, want %q", c.variant, c.value, got, c.want)
		}
	}
}

// TestFTS5QuoteEscapes guards that a term holding a double quote is escaped so the
// MATCH string stays well-formed rather than terminating the literal early.
func TestFTS5QuoteEscapes(t *testing.T) {
	if got := fts5Quote(`a"b`); got != `"a""b"` {
		t.Errorf("fts5Quote = %q, want %q", got, `"a""b"`)
	}
}

func TestFullTextLowering(t *testing.T) {
	ref := &sqlgen.FullTextRef{Table: `"films_fts"`, RowidRef: `"films"."id"`}
	frag, bind, ok := dialect{}.FullText("", ref, ir.FTSPlain, "english", "cat")
	if !ok {
		t.Fatal("FullText ok = false, want true with an index")
	}
	want := `"films"."id" IN (SELECT rowid FROM "films_fts" WHERE "films_fts" MATCH ` + sqlgen.PatternMark + `)`
	if frag != want {
		t.Errorf("frag = %q, want %q", frag, want)
	}
	if bind != `"cat"` {
		t.Errorf("bind = %q, want %q", bind, `"cat"`)
	}
}

// TestFullTextNoIndex is the missing-structure case: with no covering FTS5 table
// the dialect reports ok=false so the compiler raises PGRST127 instead of scanning.
func TestFullTextNoIndex(t *testing.T) {
	if _, _, ok := (dialect{}).FullText("col", nil, ir.FTSPlain, "", "cat"); ok {
		t.Error("FullText with a nil index ok = true, want false")
	}
}

func TestRegexFeatureGap(t *testing.T) {
	cases := map[string]bool{ // pattern -> should be flagged
		`^Foo.*bar$`: false,
		`\d+\w*`:     false,
		`(foo|bar)`:  false,
		`(a)\1`:      true, // backreference
		`(?=foo)`:    true, // lookahead
		`(?!foo)`:    true, // negative lookahead
		`(?<=foo)`:   true, // lookbehind
		`(?<!foo)`:   true, // negative lookbehind
	}
	for pat, flagged := range cases {
		got := dialect{}.RegexFeatureGap(pat) != ""
		if got != flagged {
			t.Errorf("RegexFeatureGap(%q) flagged = %v, want %v", pat, got, flagged)
		}
	}
}

func TestParseFTS5(t *testing.T) {
	decl, ok := parseFTS5("films_fts",
		`CREATE VIRTUAL TABLE films_fts USING fts5(title, body, content='films', content_rowid='id')`)
	if !ok {
		t.Fatal("parseFTS5 ok = false, want true")
	}
	if len(decl.columns) != 2 || decl.columns[0] != "title" || decl.columns[1] != "body" {
		t.Errorf("columns = %v, want [title body]", decl.columns)
	}
	if decl.content != "films" {
		t.Errorf("content = %q, want films", decl.content)
	}
	if decl.rowidCol != "id" {
		t.Errorf("rowidCol = %q, want id", decl.rowidCol)
	}
}

func TestParseFTS5Standalone(t *testing.T) {
	decl, ok := parseFTS5("docs", `CREATE VIRTUAL TABLE docs USING fts5(body, meta UNINDEXED)`)
	if !ok {
		t.Fatal("parseFTS5 ok = false, want true")
	}
	if len(decl.columns) != 2 || decl.columns[0] != "body" || decl.columns[1] != "meta" {
		t.Errorf("columns = %v, want [body meta]", decl.columns)
	}
	if decl.content != "" || decl.rowidCol != "" {
		t.Errorf("standalone should carry no content/rowid, got %q/%q", decl.content, decl.rowidCol)
	}
}

func TestParseFTS5NotFTS(t *testing.T) {
	if _, ok := parseFTS5("films", `CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT)`); ok {
		t.Error("parseFTS5 on a plain table ok = true, want false")
	}
}

// openFTS builds a films table mirrored into an external-content FTS5 index and
// populates both. The FTS5 table is loaded directly (no triggers) since the rows
// are static for the test.
func openFTS(t *testing.T) *Backend {
	t.Helper()
	b := openSeeded(t)
	_, err := b.DB().Exec(`
		CREATE VIRTUAL TABLE films_fts USING fts5(title, content='films', content_rowid='id');
		INSERT INTO films_fts (rowid, title) SELECT id, title FROM films;
	`)
	if err != nil {
		t.Fatalf("create fts: %v", err)
	}
	return b
}

// TestIntrospectHidesFTS checks the FTS5 virtual table and its shadow tables are
// not exposed as relations, and the index is attached to the base table.
func TestIntrospectHidesFTS(t *testing.T) {
	b := openFTS(t)
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if _, ok := model.Lookup("films_fts", nil); ok {
		t.Error("films_fts should be hidden from the exposed schema")
	}
	for _, suf := range ftsShadowSuffixes {
		if _, ok := model.Lookup("films_fts"+suf, nil); ok {
			t.Errorf("shadow table films_fts%s should be hidden", suf)
		}
	}
	rel, ok := model.Lookup("films", nil)
	if !ok {
		t.Fatal("films not found")
	}
	idx := rel.FullTextIndexFor("title")
	if idx == nil {
		t.Fatal("no full-text index attached for films.title")
	}
	if idx.Name != "films_fts" || idx.RowidColumn != "id" {
		t.Errorf("index = %+v, want films_fts/id", idx)
	}
}

// planRead resolves a parsed query against the live model so the FTS index lands
// on the predicate, the way the request pipeline does.
func planRead(t *testing.T, b *Backend, query string) *ir.Plan {
	t.Helper()
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	q, perr := ir.ParseRead("films", query, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	pl, perr := plan.Read(model, q, nil)
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	return pl
}

// TestFTSEndToEnd runs an fts filter through introspection, planning, and the
// SQLite backend, confirming the FTS5 MATCH selects the expected row.
func TestFTSEndToEnd(t *testing.T) {
	b := openFTS(t)
	pl := planRead(t, b, "title=fts.metropolis")
	rc := &reqctx.Context{Role: "anon", Method: "GET"}
	res, err := b.Execute(context.Background(), pl, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rows := readAll(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["title"] != "Metropolis" {
		t.Errorf("title = %v, want Metropolis", rows[0]["title"])
	}
}

// TestFTSMissingIndexErrors checks an fts filter on a column with no FTS5 table is
// a clean PGRST127 naming the column, not a silent scan.
func TestFTSMissingIndexErrors(t *testing.T) {
	b := openSeeded(t) // no FTS5 table created
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	q, perr := ir.ParseRead("films", "title=fts.metropolis", nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	pl, perr := plan.Read(model, q, nil)
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	rc := &reqctx.Context{Role: "anon", Method: "GET"}
	_, err = b.Execute(context.Background(), pl, rc)
	if pe := pgerr.As(err); pe == nil || pe.Code != pgerr.CodeUnsupported {
		t.Fatalf("error = %v, want PGRST127", err)
	}
}

// TestRegexBackreferenceErrors checks a backreference pattern is rejected before
// lowering with PGRST127 rather than failing deep in the RE2 engine.
func TestRegexBackreferenceErrors(t *testing.T) {
	b := openSeeded(t)
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	q, perr := ir.ParseRead("films", `title=match.(a)\1`, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	pl, perr := plan.Read(model, q, nil)
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	rc := &reqctx.Context{Role: "anon", Method: "GET"}
	_, err = b.Execute(context.Background(), pl, rc)
	if pe := pgerr.As(err); pe == nil || pe.Code != pgerr.CodeUnsupported {
		t.Fatalf("error = %v, want PGRST127", err)
	}
}

func BenchmarkFTS5Query(bb *testing.B) {
	bb.ReportAllocs()
	for bb.Loop() {
		_ = fts5Query(ir.FTSPlain, "cat & hat | dog")
	}
}
