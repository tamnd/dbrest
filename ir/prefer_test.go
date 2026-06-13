package ir

import (
	"strings"
	"testing"
)

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

// TestParsePreferMaxAffectedStrictOnly checks max-affected=N is parsed and
// echoed only under handling=strict; under lenient PostgREST ignores it, so both
// the value and the echo are dropped.
func TestParsePreferMaxAffectedStrictOnly(t *testing.T) {
	strict := ParsePrefer([]string{"handling=strict, max-affected=5"})
	if strict.MaxAffected == nil || *strict.MaxAffected != 5 {
		t.Fatalf("strict MaxAffected = %v, want 5", strict.MaxAffected)
	}
	if got, want := strict.AppliedHeader(), "handling=strict, max-affected=5"; got != want {
		t.Errorf("strict AppliedHeader = %q, want %q", got, want)
	}

	lenient := ParsePrefer([]string{"max-affected=5"})
	if lenient.MaxAffected != nil {
		t.Errorf("lenient MaxAffected = %v, want nil (ignored)", *lenient.MaxAffected)
	}
	if got := lenient.AppliedHeader(); got != "" {
		t.Errorf("lenient AppliedHeader = %q, want empty", got)
	}
}

// TestParsePreferMaxAffectedBadValue checks a non-integer or negative
// max-affected is an offender (PGRST122 under strict) and leaves the bound unset.
func TestParsePreferMaxAffectedBadValue(t *testing.T) {
	for _, v := range []string{"abc", "-1", "1.5", ""} {
		p := ParsePrefer([]string{"handling=strict, max-affected=" + v})
		if p.MaxAffected != nil {
			t.Errorf("max-affected=%q set MaxAffected = %v, want nil", v, *p.MaxAffected)
		}
		if p.StrictError() == nil {
			t.Errorf("max-affected=%q under strict should be a PGRST122 offender", v)
		}
	}
}

// TestParsePreferTimeZoneValid checks a valid IANA name is captured and echoed,
// unlike max-affected it is honored under lenient too.
func TestParsePreferTimeZoneValid(t *testing.T) {
	for _, h := range []string{"timezone=America/Los_Angeles", "handling=strict, timezone=America/Los_Angeles"} {
		p := ParsePrefer([]string{h})
		if p.TimeZone == nil || *p.TimeZone != "America/Los_Angeles" {
			t.Fatalf("%q: TimeZone = %v, want America/Los_Angeles", h, p.TimeZone)
		}
		if p.StrictError() != nil {
			t.Errorf("%q: valid timezone should not be an offender", h)
		}
		// The Preference-Applied echo carries the honored timezone token.
		if !strings.Contains(p.AppliedHeader(), "timezone=America/Los_Angeles") {
			t.Errorf("%q: AppliedHeader = %q, missing timezone echo", h, p.AppliedHeader())
		}
	}
}

// TestParsePreferTimeZoneInvalid checks an unknown or empty zone is an offender:
// ignored (no echo) under lenient, a PGRST122 under strict.
func TestParsePreferTimeZoneInvalid(t *testing.T) {
	for _, v := range []string{"Mars/Phobos", "Not_A_Zone", ""} {
		lenient := ParsePrefer([]string{"timezone=" + v})
		if lenient.TimeZone != nil {
			t.Errorf("timezone=%q set TimeZone = %v, want nil", v, *lenient.TimeZone)
		}
		if lenient.AppliedHeader() != "" {
			t.Errorf("timezone=%q lenient AppliedHeader = %q, want empty", v, lenient.AppliedHeader())
		}
		strict := ParsePrefer([]string{"handling=strict, timezone=" + v})
		if strict.StrictError() == nil {
			t.Errorf("timezone=%q under strict should be a PGRST122 offender", v)
		}
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
