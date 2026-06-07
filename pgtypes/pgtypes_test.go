package pgtypes

import "testing"

func TestNormalizeFoldsAliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"int4", "int4"},
		{"integer", "int4"},
		{"INTEGER", "int4"},
		{"  bigint ", "int8"},
		{"smallint", "int2"},
		{"serial", "int4"},
		{"double precision", "float8"},
		{"real", "float4"},
		{"decimal", "numeric"},
		{"boolean", "bool"},
		{"BOOL", "bool"},
		{"character varying", "varchar"},
		{"bpchar", "text"},
		{"timestamp with time zone", "timestamptz"},
		{"timestamp without time zone", "timestamp"},
		{"jsonb", "jsonb"},
	}
	for _, c := range cases {
		got, ok := Normalize(c.in)
		if !ok {
			t.Errorf("Normalize(%q) ok = false, want true", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeUnknownIsNotOK(t *testing.T) {
	got, ok := Normalize("widget")
	if ok {
		t.Errorf("Normalize(widget) ok = true, want false")
	}
	if got != "widget" {
		t.Errorf("Normalize(widget) = %q, want the input back", got)
	}
}

func TestClassOf(t *testing.T) {
	cases := map[string]Class{
		"int4":             ClassInteger,
		"bigint":           ClassInteger,
		"float8":           ClassFloat,
		"double precision": ClassFloat,
		"numeric":          ClassNumeric,
		"bool":             ClassBool,
		"text":             ClassText,
		"varchar":          ClassText,
		"timestamptz":      ClassTemporal,
		"date":             ClassTemporal,
		"uuid":             ClassUUID,
		"jsonb":            ClassJSON,
		"bytea":            ClassBytea,
		"widget":           ClassOther,
	}
	for in, want := range cases {
		if got := ClassOf(in); got != want {
			t.Errorf("ClassOf(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestKnown(t *testing.T) {
	if !Known("integer") {
		t.Errorf("Known(integer) = false, want true")
	}
	if Known("widget") {
		t.Errorf("Known(widget) = true, want false")
	}
}
