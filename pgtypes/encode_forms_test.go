package pgtypes

import (
	"encoding/json"
	"testing"
)

// The render helpers exist because one canonical type arrives in several
// physical forms depending on the engine and its driver: an integer as int64,
// int, int32, float64, or decimal text; a numeric as float, int, or exact text;
// a timestamp as time.Time or pre-formatted text. RenderJSON must fold them all
// to the same bytes (spec 16, "Codecs"; spec 22, byte-identical bodies), so each
// form is exercised rather than only the one a given backend happens to send.

func TestRenderIntegerEveryForm(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int64(42), "42"},
		{int(7), "7"},
		{int32(-3), "-3"},
		{float64(100), "100"}, // a whole number from a float column truncates cleanly
		{[]byte("256"), "256"},
		{"512", "512"},
	}
	for _, c := range cases {
		b, err := RenderJSON("int8", c.in)
		if err != nil || string(b) != c.want {
			t.Errorf("RenderJSON(int8, %#v) = %q, %v; want %s", c.in, b, err, c.want)
		}
	}
}

func TestRenderIntegerRejectsNonNumber(t *testing.T) {
	if _, err := RenderJSON("int4", "not-a-number"); err == nil {
		t.Error("a non-decimal string should be a CoerceError, not silent")
	}
	if _, err := RenderJSON("int4", []byte("0x10")); err == nil {
		t.Error("a non-decimal byte string should be a CoerceError")
	}
	if _, err := RenderJSON("int4", struct{}{}); err == nil {
		t.Error("an unhandled Go type should be a CoerceError")
	}
}

func TestRenderNumberEveryForm(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(3.5), "3.5"},
		{float32(2.5), "2.5"},
		{int64(9), "9"},
		{[]byte("1.25"), "1.25"},
		{"0.001", "0.001"},
	}
	for _, c := range cases {
		b, err := RenderJSON("float8", c.in)
		if err != nil || string(b) != c.want {
			t.Errorf("RenderJSON(float8, %#v) = %q, %v; want %s", c.in, b, err, c.want)
		}
	}
}

func TestRenderNumberRejectsNonNumber(t *testing.T) {
	_, err := RenderJSON("numeric", "abc")
	if err == nil {
		t.Fatal("a non-decimal numeric should be a CoerceError")
	}
	var ce *CoerceError
	if !asCoerce(err, &ce) || ce.Canonical != "numeric" {
		t.Errorf("error = %v, want a CoerceError naming numeric", err)
	}
}

func TestRenderTemporalTextForms(t *testing.T) {
	// A driver that hands back an already-formatted timestamp (a []byte or string)
	// is passed through unchanged rather than re-parsed.
	for _, in := range []any{[]byte("2026-06-06T14:30:00Z"), "2026-06-06T14:30:00Z"} {
		b, err := RenderJSON("timestamptz", in)
		if err != nil || string(b) != `"2026-06-06T14:30:00Z"` {
			t.Errorf("RenderJSON(timestamptz, %T) = %q, %v", in, b, err)
		}
	}
}

func TestRenderByteaFromString(t *testing.T) {
	// Some drivers surface a bytea as a string; it hexes the same as the bytes.
	b, err := RenderJSON("bytea", "\xde\xad")
	if err != nil || string(b) != `"\\xdead"` {
		t.Errorf("RenderJSON(bytea, string) = %q, %v; want the \\x hex", b, err)
	}
}

func TestRenderEmbeddedJSONForms(t *testing.T) {
	// Text already holding JSON is embedded as-is; any other Go value is marshaled.
	cases := []struct {
		in   any
		want string
	}{
		{[]byte(`[1,2]`), `[1,2]`},
		{`{"k":true}`, `{"k":true}`},
		{json.RawMessage(`"raw"`), `"raw"`},
		{map[string]int{"n": 1}, `{"n":1}`},
	}
	for _, c := range cases {
		b, err := RenderJSON("json", c.in)
		if err != nil || string(b) != c.want {
			t.Errorf("RenderJSON(json, %#v) = %q, %v; want %s", c.in, b, err, c.want)
		}
	}
}

func TestRenderUUIDForms(t *testing.T) {
	// A 36-char text uuid normalizes to lowercase hyphenated; a 16-byte value
	// hyphenates from its hex; a wrong-length byte slice falls back to text
	// normalization (and fails if not a uuid).
	b, err := RenderJSON("uuid", "4E9D2C7A-1B6F-4C3A-9E21-6A0F8B2D11C5")
	if err != nil || string(b) != `"4e9d2c7a-1b6f-4c3a-9e21-6a0f8b2d11c5"` {
		t.Errorf("RenderJSON(uuid, text) = %q, %v; want lowercased", b, err)
	}
	if _, err := RenderJSON("uuid", []byte("too-short")); err == nil {
		t.Error("a non-uuid byte string should be a CoerceError")
	}
	if _, err := RenderJSON("uuid", 12345); err == nil {
		t.Error("an integer is not a uuid and should be a CoerceError")
	}
}

func TestRenderBoolRejectsUnknown(t *testing.T) {
	if _, err := RenderJSON("bool", "maybe"); err == nil {
		t.Error("an unparseable bool word should be a CoerceError")
	}
	if _, err := RenderJSON("bool", struct{}{}); err == nil {
		t.Error("an unhandled type for bool should be a CoerceError")
	}
}

func TestRenderUnknownTypeFallsBackToString(t *testing.T) {
	// A type outside the known classes renders its value as a JSON string through
	// the default path: a []byte and an arbitrary value both stringify.
	b, err := RenderJSON("citext", []byte("hello"))
	if err != nil || string(b) != `"hello"` {
		t.Errorf("RenderJSON(citext, bytes) = %q, %v; want \"hello\"", b, err)
	}
	b, err = RenderJSON("citext", 7)
	if err != nil || string(b) != `"7"` {
		t.Errorf("RenderJSON(citext, int) = %q, %v; want \"7\"", b, err)
	}
}

// asCoerce reports whether err is a *CoerceError and binds it, without pulling in
// errors.As for a single concrete type.
func asCoerce(err error, dst **CoerceError) bool {
	ce, ok := err.(*CoerceError)
	if ok {
		*dst = ce
	}
	return ok
}
