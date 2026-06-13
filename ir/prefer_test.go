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

func TestAppliedHeaderEchoesHonoredInCanonicalOrder(t *testing.T) {
	// Sent count before return; PostgREST's Preference-Applied is emitted in its
	// fixed order (return before count), not request order.
	p := ParsePrefer([]string{"count=exact, return=minimal"})
	if got, want := p.AppliedHeader(), "return=minimal, count=exact"; got != want {
		t.Errorf("AppliedHeader = %q, want %q", got, want)
	}
}

// TestParsePreferFirstDuplicateWins checks only the first occurrence of a
// duplicated preference is honored, matching PostgREST, and the applied header
// carries one token.
func TestParsePreferFirstDuplicateWins(t *testing.T) {
	p := ParsePrefer([]string{"count=exact, count=planned"})
	if p.Count == nil || *p.Count != CountExact {
		t.Errorf("count = %v, want the first occurrence (exact)", p.Count)
	}
	if got, want := p.AppliedHeader(), "count=exact"; got != want {
		t.Errorf("AppliedHeader = %q, want %q", got, want)
	}
}

// TestParsePreferLenientEchoed checks an explicit handling=lenient is recognized
// and echoed, where before it was dropped as unknown.
func TestParsePreferLenientEchoed(t *testing.T) {
	p := ParsePrefer([]string{"handling=lenient"})
	if p.Handling != HandlingLenient {
		t.Errorf("handling = %v, want lenient", p.Handling)
	}
	if got, want := p.AppliedHeader(), "handling=lenient"; got != want {
		t.Errorf("AppliedHeader = %q, want %q", got, want)
	}
}

// TestStrictErrorRejectsOffenders checks handling=strict turns an unknown key or
// a bad value into a PGRST122, while the default lenient handling ignores them.
func TestStrictErrorRejectsOffenders(t *testing.T) {
	strict := ParsePrefer([]string{"handling=strict, return=bogus, frobnicate=yes"})
	err := strict.StrictError()
	if err == nil || err.Code != "PGRST122" {
		t.Fatalf("StrictError = %v, want PGRST122", err)
	}
	lenient := ParsePrefer([]string{"return=bogus, frobnicate=yes"})
	if lenient.StrictError() != nil {
		t.Error("lenient handling must not reject invalid preferences")
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
