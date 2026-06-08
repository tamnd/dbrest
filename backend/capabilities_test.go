package backend

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

func TestTierString(t *testing.T) {
	cases := map[Tier]string{Native: "N", Emulated: "E", BestEffort: "B", Unsupported: "U"}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", tier, got, want)
		}
	}
}

func TestTierOK(t *testing.T) {
	if Unsupported.OK() {
		t.Error("Unsupported.OK() should be false")
	}
	for _, tr := range []Tier{Native, Emulated, BestEffort} {
		if !tr.OK() {
			t.Errorf("%v.OK() should be true", tr)
		}
	}
}

func TestOperatorDefaultNative(t *testing.T) {
	var c Capabilities // nil Operators map
	if c.Operator(int(ir.OpEq)) != Native {
		t.Error("unset operator should default to Native")
	}
	c.Operators = map[int]Tier{int(ir.OpRangeSL): Unsupported}
	if c.Operator(int(ir.OpRangeSL)) != Unsupported {
		t.Error("overridden operator should report its tier")
	}
	if c.Operator(int(ir.OpEq)) != Native {
		t.Error("non-overridden operator should still default to Native")
	}
}

// Operator is read by the planner for every horizontal-filter operator on every
// request, so its lookup sits on the hot path. The benchmark grades an operator
// present in the override map, the case that walks the map rather than taking
// the nil-map fast return.
func BenchmarkOperatorLookup(b *testing.B) {
	c := Capabilities{Operators: map[int]Tier{
		int(ir.OpRangeSL): Unsupported,
		int(ir.OpFTS):     BestEffort,
		int(ir.OpMatch):   Emulated,
	}}
	op := int(ir.OpRangeSL)
	b.ReportAllocs()
	for b.Loop() {
		if c.Operator(op) != Unsupported {
			b.Fatal("OpRangeSL is graded Unsupported")
		}
	}
}
