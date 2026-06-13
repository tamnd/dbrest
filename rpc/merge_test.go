package rpc

import (
	"reflect"
	"sort"
	"testing"
)

// names lists every function name a registry exposes, with overloads repeated, in
// the order List returns them, so a test can assert the merged candidate set.
func names(reg Registry) []string {
	var out []string
	for _, f := range reg.List() {
		out = append(out, f.Signature(""))
	}
	return out
}

// Merge returns the non-empty side unchanged when the other is empty, so the
// common case (no declared registry alongside the native one) adds no overhead.
func TestMergeEmptySides(t *testing.T) {
	native := NewStaticRegistry([]*Function{
		{Name: "add", Params: []Param{{Name: "a"}, {Name: "b"}}},
	})
	if got := Merge(EmptyRegistry{}, native); got != Registry(native) {
		t.Errorf("Merge(empty, native) should return native unchanged")
	}
	portable := NewStaticRegistry([]*Function{{Name: "echo", Params: []Param{{Name: "x"}}}})
	if got := Merge(portable, EmptyRegistry{}); got != Registry(portable) {
		t.Errorf("Merge(portable, empty) should return portable unchanged")
	}
}

// A function declared in the primary registry with the same signature as a native
// one shadows it: the primary version is the one resolved, so an operator's
// explicit declaration wins.
func TestMergePrimaryShadowsSameSignature(t *testing.T) {
	portable := NewStaticRegistry([]*Function{
		{Name: "add", Params: []Param{{Name: "a", Type: "int"}, {Name: "b", Type: "int"}},
			Query: &PortableQuery{SQL: "SELECT :a + :b"}},
	})
	native := NewStaticRegistry([]*Function{
		{Name: "add", Params: []Param{{Name: "a", Type: "int"}, {Name: "b", Type: "int"}}},
	})
	merged := Merge(portable, native)

	got := names(merged)
	want := []string{"add(a => int, b => int)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged signatures = %v, want %v", got, want)
	}
	fn, ok := merged.Lookup("add", ArgSet{"a": true, "b": true})
	if !ok {
		t.Fatal("add should resolve in the merged registry")
	}
	if fn.Query == nil {
		t.Error("the declared (portable) add should win over the native one")
	}
}

// Overloads that differ in any parameter are kept as distinct candidates from both
// sides, and overload resolution runs across the union: the right arg set picks the
// right source.
func TestMergeKeepsDistinctOverloads(t *testing.T) {
	portable := NewStaticRegistry([]*Function{
		{Name: "f", Params: []Param{{Name: "a", Type: "text"}},
			Query: &PortableQuery{SQL: "SELECT :a"}},
	})
	native := NewStaticRegistry([]*Function{
		{Name: "f", Params: []Param{{Name: "a", Type: "text"}, {Name: "b", Type: "text"}}},
		{Name: "g", Params: []Param{{Name: "x", Type: "int"}}},
	})
	merged := Merge(portable, native)

	got := names(merged)
	sort.Strings(got)
	want := []string{"f(a => text)", "f(a => text, b => text)", "g(x => int)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged signatures = %v, want %v", got, want)
	}

	// The one-arg form is the portable overload; the two-arg form is native.
	one, ok := merged.Lookup("f", ArgSet{"a": true})
	if !ok || one.Query == nil {
		t.Errorf("f(a) should resolve to the portable overload, got %+v ok=%v", one, ok)
	}
	two, ok := merged.Lookup("f", ArgSet{"a": true, "b": true})
	if !ok || two.Query != nil {
		t.Errorf("f(a,b) should resolve to the native overload, got %+v ok=%v", two, ok)
	}
	if _, ok := merged.Lookup("g", ArgSet{"x": true}); !ok {
		t.Error("g(x) from the native side should resolve in the merged registry")
	}
}
