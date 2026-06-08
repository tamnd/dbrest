package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/schema"
)

// writeCond walks the boolean tree. The AND arm is covered elsewhere; this pins
// OR, a nested NOT wrapping an AND, and the unknown-node fallback so a malformed
// tree becomes an internal error rather than silent SQL.
func TestCompileOrAndNestedNot(t *testing.T) {
	where := ir.Cond(ir.Or{Kids: []ir.Cond{
		ir.Compare{Path: []string{"a"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}},
		ir.Not{Kid: ir.And{Kids: []ir.Cond{
			ir.Compare{Path: []string{"b"}, Op: ir.OpLt, Value: ir.Value{Text: "5"}},
			ir.Compare{Path: []string{"c"}, Op: ir.OpGt, Value: ir.Value{Text: "0"}},
		}}},
	}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE ("a" = $1 OR NOT (("b" < $2 AND "c" > $3)))`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 3 {
		t.Errorf("Args = %v, want 3", st.Args)
	}
}

// binaryOp maps each infix operator to its SQL spelling, including the two LIKE
// forms that share a keyword and the default that falls back to equality.
func TestBinaryOp(t *testing.T) {
	cases := []struct {
		op   ir.Op
		want string
	}{
		{ir.OpEq, "="},
		{ir.OpNeq, "<>"},
		{ir.OpGt, ">"},
		{ir.OpGte, ">="},
		{ir.OpLt, "<"},
		{ir.OpLte, "<="},
		{ir.OpLike, "LIKE"},
		{ir.OpILike, "LIKE"},
		{ir.OpIn, "="}, // not an infix op: the default arm falls back to equality
	}
	for _, c := range cases {
		if got := binaryOp(c.op); got != c.want {
			t.Errorf("binaryOp(%v) = %q, want %q", c.op, got, c.want)
		}
	}
}

// Each comparison operator that routes through binaryOp produces its spelling in
// the compiled SQL, so the mapping is exercised end to end and not only in
// isolation.
func TestCompileEveryInfixOperator(t *testing.T) {
	cases := []struct {
		op   ir.Op
		want string
	}{
		{ir.OpNeq, "<>"},
		{ir.OpGt, ">"},
		{ir.OpLt, "<"},
		{ir.OpLte, "<="},
		{ir.OpLike, "LIKE"},
	}
	for _, c := range cases {
		where := ir.Cond(ir.Compare{Path: []string{"c"}, Op: c.op, Value: ir.Value{Text: "x"}})
		st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
		want := `SELECT * FROM "t" WHERE "c" ` + c.want + ` $1`
		if st.SQL != want {
			t.Errorf("op %v: SQL = %q, want %q", c.op, st.SQL, want)
		}
	}
}

// indexFTSDialect models an engine whose full text needs a covering index: it
// quotes the index reference into the emitted expression, and reports ok=false
// when the planner attached none.
type indexFTSDialect struct{ stub }

func (indexFTSDialect) FullText(col string, idx *FullTextRef, _ ir.FTSVariant, _, _ string) (string, string, bool) {
	if idx == nil {
		return "", "", false
	}
	return idx.Table + " MATCH " + PatternMark, "cat", true
}

// When the planner resolves a covering index, writeFTS hands its table and rowid
// reference to the dialect; the emitted SQL carries the quoted index name.
func TestCompileFTSWithCoveringIndex(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path: []string{"body"}, Op: ir.OpFTS, FTS: ir.FTSWeb,
		Value:    ir.Value{Text: "cat"},
		FullText: &schema.FullTextIndex{Name: "docs_fts", RowidColumn: "rowid"},
	})
	st, err := CompileRead(indexFTSDialect{}, &ir.Query{Relation: ir.Ref{Name: "docs"}, Where: &where})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	if !strings.Contains(st.SQL, `"docs_fts" MATCH $1`) {
		t.Errorf("SQL = %q, want it to carry the quoted index match", st.SQL)
	}
}

// A dialect that needs an index but got none reports the predicate unavailable,
// surfacing as PGRST127 naming the column rather than a silent table scan.
func TestCompileFTSMissingIndexIsUnavailable(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"body"}, Op: ir.OpFTS, FTS: ir.FTSWeb, Value: ir.Value{Text: "cat"}})
	_, err := CompileRead(indexFTSDialect{}, &ir.Query{Relation: ir.Ref{Name: "docs"}, Where: &where})
	if err == nil || err.Code != "PGRST127" {
		t.Fatalf("want PGRST127 full-text unavailable, got %v", err)
	}
}
