package pgtypes

import (
	"testing"
	"time"
)

func TestRenderJSONNull(t *testing.T) {
	b, err := RenderJSON("int4", nil)
	if err != nil || string(b) != "null" {
		t.Errorf("RenderJSON(nil) = %q, %v; want null", b, err)
	}
}

func TestRenderJSONBoolFromManyForms(t *testing.T) {
	// A bool renders true/false regardless of the engine's physical storage: a
	// Go bool (PostgreSQL), an int64 0/1 (MySQL TINYINT(1), SQL Server BIT), or a
	// text form.
	for _, v := range []any{true, int64(1), int(1), 1.0, "t", []byte("true")} {
		b, err := RenderJSON("bool", v)
		if err != nil || string(b) != "true" {
			t.Errorf("RenderJSON(bool, %v) = %q, %v; want true", v, b, err)
		}
	}
	for _, v := range []any{false, int64(0), "f"} {
		b, err := RenderJSON("bool", v)
		if err != nil || string(b) != "false" {
			t.Errorf("RenderJSON(bool, %v) = %q, %v; want false", v, b, err)
		}
	}
}

func TestRenderJSONInteger(t *testing.T) {
	b, err := RenderJSON("int8", int64(42))
	if err != nil || string(b) != "42" {
		t.Errorf("RenderJSON(int8, 42) = %q, %v; want 42", b, err)
	}
}

func TestRenderJSONNumericExact(t *testing.T) {
	// numeric is emitted from its exact text, never routed through a float64.
	b, err := RenderJSON("numeric", "9999999999999999999999.0001")
	if err != nil || string(b) != "9999999999999999999999.0001" {
		t.Errorf("RenderJSON(numeric) = %q, %v; want the exact decimal", b, err)
	}
}

func TestRenderJSONFloat(t *testing.T) {
	b, err := RenderJSON("float8", 3.5)
	if err != nil || string(b) != "3.5" {
		t.Errorf("RenderJSON(float8, 3.5) = %q, %v; want 3.5", b, err)
	}
}

func TestRenderJSONTimestamp(t *testing.T) {
	ts := time.Date(2026, 6, 6, 14, 30, 0, 0, time.UTC)
	b, err := RenderJSON("timestamptz", ts)
	if err != nil {
		t.Fatalf("RenderJSON(timestamptz) error = %v", err)
	}
	if string(b) != `"2026-06-06T14:30:00Z"` {
		t.Errorf("RenderJSON(timestamptz) = %q, want the ISO-8601 string", b)
	}
}

func TestRenderJSONUUIDFromBytes(t *testing.T) {
	raw := []byte{0x4e, 0x9d, 0x2c, 0x7a, 0x1b, 0x6f, 0x4c, 0x3a, 0x9e, 0x21, 0x6a, 0x0f, 0x8b, 0x2d, 0x11, 0xc5}
	b, err := RenderJSON("uuid", raw)
	if err != nil {
		t.Fatalf("RenderJSON(uuid, bytes) error = %v", err)
	}
	if string(b) != `"4e9d2c7a-1b6f-4c3a-9e21-6a0f8b2d11c5"` {
		t.Errorf("RenderJSON(uuid, bytes) = %q, want the hyphenated string", b)
	}
}

func TestRenderJSONByteaHex(t *testing.T) {
	b, err := RenderJSON("bytea", []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil || string(b) != `"\\xdeadbeef"` {
		t.Errorf("RenderJSON(bytea) = %q, %v; want the \\x hex string", b, err)
	}
}

func TestRenderJSONEmbeddedJSON(t *testing.T) {
	b, err := RenderJSON("jsonb", []byte(`{"a":1}`))
	if err != nil || string(b) != `{"a":1}` {
		t.Errorf("RenderJSON(jsonb) = %q, %v; want embedded JSON", b, err)
	}
}

func TestRenderJSONText(t *testing.T) {
	b, err := RenderJSON("text", `he said "hi"`)
	if err != nil || string(b) != `"he said \"hi\""` {
		t.Errorf("RenderJSON(text) = %q, %v; want the escaped string", b, err)
	}
}

func BenchmarkRenderJSONBool(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := RenderJSON("bool", int64(1)); err != nil {
			b.Fatal(err)
		}
	}
}
