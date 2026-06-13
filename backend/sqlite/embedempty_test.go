package sqlite

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// 07.8: an empty-parenthesis embed, directors!inner(), joins the relation to
// filter the parent while projecting no embed key. With the !inner modifier and
// an embed-scoped filter, only films whose director matches survive, and the
// row carries title alone.
func TestExecuteEmptyEmbedInnerFilters(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "films", "select=title,directors!inner()&directors.name=eq.Scott&order=id")
	rows := execReadResolved(t, b, q)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only Scott's film)", len(rows))
	}
	if got, _ := asString(rows[0]["title"]); got != "Blade Runner" {
		t.Errorf("title = %q, want Blade Runner", got)
	}
	if _, present := rows[0]["directors"]; present {
		t.Errorf("row carries a directors key %#v, want it hidden", rows[0]["directors"])
	}
}

// A left empty embed hides the key without filtering: every film comes back, and
// none of them carries the embed key.
func TestExecuteEmptyEmbedLeftHidesKey(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "films", "select=title,directors()&order=id")
	rows := execReadResolved(t, b, q)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want all 3 films", len(rows))
	}
	for i, r := range rows {
		if _, present := r["directors"]; present {
			t.Errorf("row %d carries a directors key, want it hidden", i)
		}
		if _, present := r["title"]; !present {
			t.Errorf("row %d missing title", i)
		}
	}
}

// The compiled SQL for the !inner empty embed restricts the parent through an
// EXISTS but never projects the embed: no json_object and no AS for its key.
func TestCompileEmptyEmbedNoProjection(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "films", "select=title,directors!inner()&directors.name=eq.Scott")
	st, perr := sqlgen.CompileRead(dialect{}, q)
	if perr != nil {
		t.Fatalf("CompileRead: %v", perr)
	}
	if !strings.Contains(st.SQL, "EXISTS") {
		t.Errorf("SQL missing the !inner EXISTS\n got: %s", st.SQL)
	}
	if strings.Contains(st.SQL, `AS "directors"`) {
		t.Errorf("SQL projects the hidden embed key\n got: %s", st.SQL)
	}
	if strings.Contains(st.SQL, "json_object") {
		t.Errorf("SQL assembles the hidden embed object\n got: %s", st.SQL)
	}
}
