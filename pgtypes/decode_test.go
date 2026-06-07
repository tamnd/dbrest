package pgtypes

import (
	"errors"
	"testing"
)

func TestParseScalarInteger(t *testing.T) {
	v, err := ParseScalar("int4", "18")
	if err != nil {
		t.Fatalf("ParseScalar(int4, 18) error = %v", err)
	}
	if v != int64(18) {
		t.Errorf("ParseScalar(int4, 18) = %v (%T), want int64(18)", v, v)
	}
}

func TestParseScalarIntegerAlias(t *testing.T) {
	if _, err := ParseScalar("integer", "-7"); err != nil {
		t.Errorf("ParseScalar(integer, -7) error = %v", err)
	}
}

func TestParseScalarIntegerRejectsWord(t *testing.T) {
	_, err := ParseScalar("int4", "abc")
	var ce *CoerceError
	if !errors.As(err, &ce) {
		t.Fatalf("ParseScalar(int4, abc) error = %v, want *CoerceError", err)
	}
	if ce.Canonical != "int4" || ce.Input != "abc" {
		t.Errorf("CoerceError = %+v, want {int4 abc}", ce)
	}
}

func TestParseScalarInt2RangeChecked(t *testing.T) {
	// 40000 fits int4 but overflows int2.
	if _, err := ParseScalar("int2", "40000"); err == nil {
		t.Errorf("ParseScalar(int2, 40000) error = nil, want overflow")
	}
	if _, err := ParseScalar("int4", "40000"); err != nil {
		t.Errorf("ParseScalar(int4, 40000) error = %v", err)
	}
}

func TestParseScalarFloat(t *testing.T) {
	v, err := ParseScalar("float8", "3.5")
	if err != nil {
		t.Fatalf("ParseScalar(float8, 3.5) error = %v", err)
	}
	if v != 3.5 {
		t.Errorf("ParseScalar(float8, 3.5) = %v, want 3.5", v)
	}
	if _, err := ParseScalar("float8", "nope"); err == nil {
		t.Errorf("ParseScalar(float8, nope) error = nil, want failure")
	}
}

func TestParseScalarNumericPreservesText(t *testing.T) {
	// numeric is carried as text so precision is not routed through a float64.
	v, err := ParseScalar("numeric", "9999999999999999999999.0001")
	if err != nil {
		t.Fatalf("ParseScalar(numeric) error = %v", err)
	}
	if v != "9999999999999999999999.0001" {
		t.Errorf("ParseScalar(numeric) = %v, want the exact text", v)
	}
	if _, err := ParseScalar("numeric", "1.2.3"); err == nil {
		t.Errorf("ParseScalar(numeric, 1.2.3) error = nil, want failure")
	}
}

func TestParseScalarNumericForms(t *testing.T) {
	for _, ok := range []string{"0", "-1", "+1", "1.", ".5", "1.5", "1e3", "1.5E-2", "NaN", "Infinity", "-inf"} {
		if _, err := ParseScalar("numeric", ok); err != nil {
			t.Errorf("ParseScalar(numeric, %q) error = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "1e", "1e+", "abc", "1..2", "+", "-"} {
		if _, err := ParseScalar("numeric", bad); err == nil {
			t.Errorf("ParseScalar(numeric, %q) error = nil, want failure", bad)
		}
	}
}

func TestParseScalarBool(t *testing.T) {
	truthy := []string{"t", "true", "TRUE", "yes", "on", "1"}
	for _, s := range truthy {
		v, err := ParseScalar("bool", s)
		if err != nil || v != true {
			t.Errorf("ParseScalar(bool, %q) = %v, %v; want true, nil", s, v, err)
		}
	}
	falsy := []string{"f", "false", "no", "off", "0"}
	for _, s := range falsy {
		v, err := ParseScalar("bool", s)
		if err != nil || v != false {
			t.Errorf("ParseScalar(bool, %q) = %v, %v; want false, nil", s, v, err)
		}
	}
	if _, err := ParseScalar("bool", "maybe"); err == nil {
		t.Errorf("ParseScalar(bool, maybe) error = nil, want failure")
	}
}

func TestParseScalarUUIDNormalizes(t *testing.T) {
	v, err := ParseScalar("uuid", "4E9D2C7A-1B6F-4C3A-9E21-6A0F8B2D11C5")
	if err != nil {
		t.Fatalf("ParseScalar(uuid) error = %v", err)
	}
	if v != "4e9d2c7a-1b6f-4c3a-9e21-6a0f8b2d11c5" {
		t.Errorf("ParseScalar(uuid) = %v, want the lowercase hyphenated form", v)
	}
}

func TestParseScalarUUIDBareAndBraced(t *testing.T) {
	want := "4e9d2c7a-1b6f-4c3a-9e21-6a0f8b2d11c5"
	for _, in := range []string{
		"4e9d2c7a1b6f4c3a9e216a0f8b2d11c5",
		"{4e9d2c7a-1b6f-4c3a-9e21-6a0f8b2d11c5}",
	} {
		v, err := ParseScalar("uuid", in)
		if err != nil || v != want {
			t.Errorf("ParseScalar(uuid, %q) = %v, %v; want %q", in, v, err, want)
		}
	}
	for _, bad := range []string{"not-a-uuid", "4e9d2c7a-1b6f-4c3a-9e21", "zzzz2c7a1b6f4c3a9e216a0f8b2d11c5"} {
		if _, err := ParseScalar("uuid", bad); err == nil {
			t.Errorf("ParseScalar(uuid, %q) error = nil, want failure", bad)
		}
	}
}

func TestParseScalarTextCarriesThrough(t *testing.T) {
	// text and the temporal/json/bytea classes are left to the engine: any
	// operand is accepted unchanged.
	for _, ty := range []string{"text", "timestamptz", "date", "json", "bytea", "widget"} {
		v, err := ParseScalar(ty, "anything at all")
		if err != nil {
			t.Errorf("ParseScalar(%q) error = %v, want carry-through", ty, err)
		}
		if v != "anything at all" {
			t.Errorf("ParseScalar(%q) = %v, want the input back", ty, v)
		}
	}
}

func TestCoerceErrorMessage(t *testing.T) {
	e := &CoerceError{Canonical: "int4", Input: "abc"}
	want := `invalid input syntax for type int4: "abc"`
	if e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
}

func BenchmarkParseScalarInteger(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseScalar("int4", "12345"); err != nil {
			b.Fatal(err)
		}
	}
}
