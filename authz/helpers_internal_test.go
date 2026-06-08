package authz

import (
	"encoding/json"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// scalarString renders each JSON scalar form a decoded claim or payload value
// can take. The number forms matter most: a float-typed integer must print as
// "42", not "42.000000", so a claim compares equal to the text in the payload.
func TestScalarString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "alice", "alice"},
		{"json-number", json.Number("42"), "42"},
		{"float-integer", float64(42), "42"},
		{"float-fraction", float64(3.5), "3.5"},
		{"bool-true", true, "true"},
		{"bool-false", false, "false"},
		{"nil", nil, ""},
		{"fallback", []int{1, 2}, "[1 2]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scalarString(c.in); got != c.want {
				t.Errorf("scalarString(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// writeColumnActions lists the actions whose column grant gates a write
// payload. Read and delete carry no payload columns, so they gate nothing.
func TestWriteColumnActions(t *testing.T) {
	cases := []struct {
		kind ir.QueryKind
		want []Action
	}{
		{ir.Insert, []Action{Insert}},
		{ir.Update, []Action{Update}},
		{ir.Upsert, []Action{Insert, Update}},
		{ir.Delete, nil},
		{ir.Read, nil},
	}
	for _, c := range cases {
		got := writeColumnActions(c.kind)
		if len(got) != len(c.want) {
			t.Errorf("writeColumnActions(%v) = %v, want %v", c.kind, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("writeColumnActions(%v) = %v, want %v", c.kind, got, c.want)
				break
			}
		}
	}
}

// valueToString prefers a decoded JSON scalar over the raw text form, so a typed
// payload value compares the same way a claim does; a value with no JSON falls
// back to its text.
func TestValueToString(t *testing.T) {
	if got := valueToString(ir.Value{JSON: json.Number("42")}); got != "42" {
		t.Errorf("valueToString(JSON 42) = %q, want 42", got)
	}
	if got := valueToString(ir.Value{Text: "alice"}); got != "alice" {
		t.Errorf("valueToString(text) = %q, want alice", got)
	}
}

// baseColumn returns a select item's base column, the first path element, and an
// empty string for a pathless item rather than panicking.
func TestBaseColumn(t *testing.T) {
	if got := baseColumn(ir.Column{Path: []string{"owner", "tenant"}}); got != "owner" {
		t.Errorf("baseColumn = %q, want owner", got)
	}
	if got := baseColumn(ir.Column{}); got != "" {
		t.Errorf("baseColumn(pathless) = %q, want empty", got)
	}
}

// actionsFor maps a query kind to the privilege actions it performs. An upsert
// is both an insert and an update; an unknown kind grants nothing.
func TestActionsFor(t *testing.T) {
	cases := []struct {
		kind ir.QueryKind
		want []Action
	}{
		{ir.Read, []Action{Select}},
		{ir.Insert, []Action{Insert}},
		{ir.Update, []Action{Update}},
		{ir.Delete, []Action{Delete}},
		{ir.Upsert, []Action{Insert, Update}},
		{ir.QueryKind(99), nil},
	}
	for _, c := range cases {
		got := actionsFor(c.kind)
		if len(got) != len(c.want) {
			t.Errorf("actionsFor(%v) = %v, want %v", c.kind, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("actionsFor(%v) = %v, want %v", c.kind, got, c.want)
				break
			}
		}
	}
}
