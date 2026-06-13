package sqlgen

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// The five range operators (sl/sr/nxr/nxl/adj) lower through the dialect's
// RangeOp hook to the native PostgreSQL spellings (item 07.5). The stub models a
// range-capable engine, so each compiles to "col <op> $1".
func TestCompileRangeOperators(t *testing.T) {
	cases := []struct {
		op   ir.Op
		want string
	}{
		{ir.OpRangeSL, "<<"},
		{ir.OpRangeSR, ">>"},
		{ir.OpRangeNXR, "&<"},
		{ir.OpRangeNXL, "&>"},
		{ir.OpRangeAdj, "-|-"},
	}
	for _, c := range cases {
		where := ir.Cond(ir.Compare{Path: []string{"period"}, Op: c.op, Value: ir.Value{Text: "[2000-01-01,2000-12-31]"}})
		st := compile(t, &ir.Query{Relation: ir.Ref{Name: "events"}, Where: &where})
		want := `SELECT * FROM "events" WHERE "period" ` + c.want + ` $1`
		if st.SQL != want {
			t.Errorf("op %v: SQL = %q, want %q", c.op, st.SQL, want)
		}
		if len(st.Args) != 1 || st.Args[0] != "[2000-01-01,2000-12-31]" {
			t.Errorf("op %v: Args = %v, want one range literal", c.op, st.Args)
		}
	}
}

// noRangeDialect models an engine without range types: its RangeOp declines, so a
// range filter is PGRST127 naming the operator rather than invalid SQL. The
// decline path is asserted in compile_test.go TestCompileRangeOperatorRejectedNamed.
type noRangeDialect struct{ stub }

func (noRangeDialect) RangeOp(_, _, _ string) (string, bool) { return "", false }
