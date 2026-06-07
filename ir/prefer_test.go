package ir

import "testing"

func TestParsePreferRecognizesEachToken(t *testing.T) {
	p := ParsePrefer([]string{"return=representation, resolution=merge-duplicates, missing=null, tx=rollback, handling=strict"})
	if p.Return == nil || *p.Return != ReturnRepresentation {
		t.Errorf("return = %v", p.Return)
	}
	if p.Resolution == nil || *p.Resolution != ConflictMerge {
		t.Errorf("resolution = %v", p.Resolution)
	}
	if p.Missing == nil || *p.Missing != MissingNull {
		t.Errorf("missing = %v", p.Missing)
	}
	if p.Tx == nil || *p.Tx != TxRollback {
		t.Errorf("tx = %v", p.Tx)
	}
	if p.Handling != HandlingStrict {
		t.Errorf("handling = %v", p.Handling)
	}
}

func TestAppliedHeaderEchoesHonoredInOrder(t *testing.T) {
	p := ParsePrefer([]string{"count=exact, return=minimal"})
	// Only honored tokens appear, in request order; the comma-joined form is the
	// Preference-Applied response header.
	if got, want := p.AppliedHeader(), "count=exact, return=minimal"; got != want {
		t.Errorf("AppliedHeader = %q, want %q", got, want)
	}
}

func TestAppliedHeaderSkipsUnknownAndEmpty(t *testing.T) {
	p := ParsePrefer([]string{"return=bogus, frobnicate=yes, count=exact"})
	if got, want := p.AppliedHeader(), "count=exact"; got != want {
		t.Errorf("AppliedHeader = %q, want %q", got, want)
	}
	empty := ParsePrefer(nil)
	if got := empty.AppliedHeader(); got != "" {
		t.Errorf("empty AppliedHeader = %q, want empty", got)
	}
}
