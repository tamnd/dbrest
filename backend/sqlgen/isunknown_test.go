package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// 07.4 task 1: is.unknown lowers to the three-valued test. The stub dialect has
// no native spelling (IsUnknown returns ok=false), so the compiler falls back to
// "col IS NULL", which selects the same rows for a boolean column.
func TestCompileIsUnknownFallback(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"done"}, Op: ir.OpIs, Value: ir.Value{Text: "unknown"}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if !strings.Contains(st.SQL, `"done" IS NULL`) {
		t.Errorf("SQL = %q, want a `done IS NULL` predicate", st.SQL)
	}
}

// A dialect that spells the operator natively (IsUnknown returns ok=true) keeps
// it, mirroring the IsBool seam: this stub stands in for PostgreSQL.
type unknownDialect struct{ stub }

func (unknownDialect) IsUnknown(col string) (string, bool) { return col + " IS UNKNOWN", true }

func TestCompileIsUnknownNative(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"done"}, Op: ir.OpIs, Value: ir.Value{Text: "unknown"}})
	st, err := CompileRead(unknownDialect{}, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	if !strings.Contains(st.SQL, `"done" IS UNKNOWN`) {
		t.Errorf("SQL = %q, want a native `done IS UNKNOWN` predicate", st.SQL)
	}
}

// 07.4 task 2: eq.true binds a boolean against a boolean column...
func TestCompileEqTrueBooleanColumn(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path: []string{"done"}, Op: ir.OpEq, ColumnType: "bool", Value: ir.Value{Text: "true"},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if !strings.Contains(st.SQL, `"done" = TRUE`) {
		t.Errorf("SQL = %q, want `done = TRUE`", st.SQL)
	}
	if len(st.Args) != 0 {
		t.Errorf("Args = %v, want none (boolean rendered inline)", st.Args)
	}
}

// ...but binds the literal word against a text column, where "true" is data, not
// a boolean, so a text column holding the word still matches.
func TestCompileEqTrueTextColumn(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path: []string{"label"}, Op: ir.OpEq, ColumnType: "text", Value: ir.Value{Text: "true"},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if strings.Contains(st.SQL, "TRUE") {
		t.Errorf("SQL = %q, want the word bound as a parameter, not the boolean TRUE", st.SQL)
	}
	if len(st.Args) != 1 || st.Args[0] != "true" {
		t.Errorf("Args = %v, want [true] bound as text", st.Args)
	}
}

// An unknown column type keeps the boolean rendering: the common filter against
// a boolean column whose type the planner did not stamp must not regress.
func TestCompileEqTrueUnknownColumnType(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"done"}, Op: ir.OpEq, Value: ir.Value{Text: "true"}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if !strings.Contains(st.SQL, `"done" = TRUE`) {
		t.Errorf("SQL = %q, want `done = TRUE` for an untyped column", st.SQL)
	}
}
