package postgres

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// 07.13: a native (non-registry) RPC with count=exact used to crash, because the
// count path ran the registry count compiler on a nil function. The native count
// is now built in the backend by wrapping the same call the row query runs.

// TestCompileNativeCallCountWrapsCall: the native count is a count(*) over the
// SELECT * FROM fn(...) the row statement issues.
func TestCompileNativeCallCountWrapsCall(t *testing.T) {
	b := &Backend{searchPath: []string{"public"}}
	c := &ir.Call{Function: ir.Ref{Name: "recent_films"}}

	row, apiErr := b.compileNativeCall(c, "public", nil)
	if apiErr != nil {
		t.Fatalf("compileNativeCall: %v", apiErr)
	}
	cnt, apiErr := b.compileNativeCallCount(c, "public", nil)
	if apiErr != nil {
		t.Fatalf("compileNativeCallCount: %v", apiErr)
	}

	want := "SELECT count(*) FROM (" + row.SQL + ") _rpc"
	if cnt.SQL != want {
		t.Errorf("count SQL = %q, want %q", cnt.SQL, want)
	}
	if !strings.Contains(cnt.SQL, `"public"."recent_films"`) {
		t.Errorf("count SQL missing schema-qualified call: %q", cnt.SQL)
	}
}

// TestCompileNativeCallCountWithArgs: the wrapper carries the call arguments
// through as embedded literals, the same as the row statement.
func TestCompileNativeCallCountWithArgs(t *testing.T) {
	b := &Backend{searchPath: []string{"app"}}
	c := &ir.Call{
		Function: ir.Ref{Name: "search"},
		Args:     map[string]ir.Value{"q": {Text: "blade"}},
	}
	cnt, apiErr := b.compileNativeCallCount(c, "app", nil)
	if apiErr != nil {
		t.Fatalf("compileNativeCallCount: %v", apiErr)
	}
	if !strings.HasPrefix(cnt.SQL, "SELECT count(*) FROM (SELECT * FROM ") {
		t.Errorf("count SQL prefix = %q", cnt.SQL)
	}
	if !strings.Contains(cnt.SQL, "'blade'") {
		t.Errorf("count SQL missing argument literal: %q", cnt.SQL)
	}
}

// 03-P02: a VOLATILE function must run exactly once, so a POST with count=exact
// cannot count with a separate statement (that would invoke the function twice and
// double its side effects). The counted wrap rides count(*) OVER () on the row
// query; the total is read off any returned row and the column dropped.

// TestCompileNativeCallCountedWrapRidesWindow: the counted wrap projects the call
// columns plus count(*) OVER () AS "_pgrst_count", in one statement.
func TestCompileNativeCallCountedWrapRidesWindow(t *testing.T) {
	b := &Backend{searchPath: []string{"public"}}
	c := &ir.Call{Function: ir.Ref{Name: "enroll_and_list"}}

	inner, apiErr := b.compileNativeCall(c, "public", nil)
	if apiErr != nil {
		t.Fatalf("compileNativeCall: %v", apiErr)
	}
	st, apiErr := sqlgen.CompileNativeCallCountedWrap(Dialect{}, c, inner)
	if apiErr != nil {
		t.Fatalf("CompileNativeCallCountedWrap: %v", apiErr)
	}
	want := `SELECT *, count(*) OVER () AS "_pgrst_count" FROM (` + inner.SQL + `) _rpc`
	if st.SQL != want {
		t.Errorf("SQL = %q\nwant   %q", st.SQL, want)
	}
}

// TestCompileNativeCallCountedWrapPostFilters: the page-shaping select, filter, and
// window apply to the wrapped call exactly as the uncounted wrap, and count(*) OVER
// () still counts the full filtered set because it is evaluated before the LIMIT.
func TestCompileNativeCallCountedWrapPostFilters(t *testing.T) {
	b := &Backend{searchPath: []string{"public"}}
	limit := 2
	c := &ir.Call{
		Function: ir.Ref{Name: "make_films"},
		Select:   []ir.SelectItem{col("title")},
		Limit:    &limit,
	}
	inner, apiErr := b.compileNativeCall(c, "public", nil)
	if apiErr != nil {
		t.Fatalf("compileNativeCall: %v", apiErr)
	}
	st, apiErr := sqlgen.CompileNativeCallCountedWrap(Dialect{}, c, inner)
	if apiErr != nil {
		t.Fatalf("CompileNativeCallCountedWrap: %v", apiErr)
	}
	want := `SELECT "title", count(*) OVER () AS "_pgrst_count" FROM (` + inner.SQL + `) _rpc LIMIT 2`
	if st.SQL != want {
		t.Errorf("SQL = %q\nwant   %q", st.SQL, want)
	}
}

// TestExtractCountWindow: the helper reads the repeated total off the first row and
// drops the count column from the columns and every row, leaving the body shape the
// renderer expects.
func TestExtractCountWindow(t *testing.T) {
	cols := []string{"id", "title", sqlgen.CountColName}
	buf := [][]any{
		{int64(1), "Dune", int64(2)},
		{int64(2), "Arrival", int64(2)},
	}
	gotCols, gotRows, total := extractCountWindow(cols, buf)
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if !reflect.DeepEqual(gotCols, []string{"id", "title"}) {
		t.Errorf("cols = %v, want [id title]", gotCols)
	}
	want := [][]any{{int64(1), "Dune"}, {int64(2), "Arrival"}}
	if !reflect.DeepEqual(gotRows, want) {
		t.Errorf("rows = %v, want %v", gotRows, want)
	}
}

// An empty result carries no row to read the window off, so the total is zero and
// the count column is still dropped from the column list.
func TestExtractCountWindowEmpty(t *testing.T) {
	cols := []string{"id", sqlgen.CountColName}
	gotCols, gotRows, total := extractCountWindow(cols, nil)
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if !reflect.DeepEqual(gotCols, []string{"id"}) {
		t.Errorf("cols = %v, want [id]", gotCols)
	}
	if len(gotRows) != 0 {
		t.Errorf("rows = %v, want empty", gotRows)
	}
}

// Without the window column the helper is a no-op: a function whose result was not
// compiled with the counted wrap passes through unchanged.
func TestExtractCountWindowAbsent(t *testing.T) {
	cols := []string{"id", "title"}
	buf := [][]any{{int64(1), "Dune"}}
	gotCols, gotRows, total := extractCountWindow(cols, buf)
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if !reflect.DeepEqual(gotCols, cols) || !reflect.DeepEqual(gotRows, buf) {
		t.Errorf("expected pass-through, got cols=%v rows=%v", gotCols, gotRows)
	}
}
