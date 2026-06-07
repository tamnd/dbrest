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
