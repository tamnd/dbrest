package rpc

import (
	"reflect"
	"testing"
)

func TestVolatilityReadOnly(t *testing.T) {
	if Volatile.ReadOnly() {
		t.Error("volatile must not be read-only")
	}
	if !Stable.ReadOnly() || !Immutable.ReadOnly() {
		t.Error("stable and immutable must be read-only")
	}
}

func TestRequiredAndParam(t *testing.T) {
	f := &Function{Params: []Param{
		{Name: "a"},
		{Name: "b", Optional: true},
		{Name: "c"},
	}}
	if got := f.Required(); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("Required = %v, want [a c]", got)
	}
	if p, ok := f.Param("b"); !ok || !p.Optional {
		t.Errorf("Param(b) = %+v, %v", p, ok)
	}
	if _, ok := f.Param("zzz"); ok {
		t.Error("Param(zzz) should miss")
	}
}

func TestStaticRegistryLookupExact(t *testing.T) {
	add := &Function{Name: "add", Params: []Param{{Name: "a"}, {Name: "b"}}}
	reg := NewStaticRegistry([]*Function{add})

	f, ok := reg.Lookup("add", ArgSet{"a": true, "b": true})
	if !ok || f != add {
		t.Fatalf("Lookup add(a,b) = %v, %v", f, ok)
	}
}

func TestStaticRegistryLookupMissingRequired(t *testing.T) {
	add := &Function{Name: "add", Params: []Param{{Name: "a"}, {Name: "b"}}}
	reg := NewStaticRegistry([]*Function{add})

	if _, ok := reg.Lookup("add", ArgSet{"a": true}); ok {
		t.Error("an absent required argument must miss")
	}
}

func TestStaticRegistryLookupStrayArg(t *testing.T) {
	add := &Function{Name: "add", Params: []Param{{Name: "a"}, {Name: "b"}}}
	reg := NewStaticRegistry([]*Function{add})

	if _, ok := reg.Lookup("add", ArgSet{"a": true, "b": true, "c": true}); ok {
		t.Error("an argument naming no parameter must miss")
	}
}

func TestStaticRegistryOptionalParam(t *testing.T) {
	f := &Function{Name: "g", Params: []Param{{Name: "a"}, {Name: "b", Optional: true}}}
	reg := NewStaticRegistry([]*Function{f})

	if _, ok := reg.Lookup("g", ArgSet{"a": true}); !ok {
		t.Error("an omitted optional parameter must still match")
	}
	if _, ok := reg.Lookup("g", ArgSet{"a": true, "b": true}); !ok {
		t.Error("a supplied optional parameter must match")
	}
}

func TestStaticRegistryOverloadSelection(t *testing.T) {
	one := &Function{Name: "f", Params: []Param{{Name: "a"}}}
	two := &Function{Name: "f", Params: []Param{{Name: "a"}, {Name: "b"}}}
	reg := NewStaticRegistry([]*Function{one, two})

	if f, ok := reg.Lookup("f", ArgSet{"a": true}); !ok || f != one {
		t.Errorf("f(a) should pick the one-arg overload")
	}
	if f, ok := reg.Lookup("f", ArgSet{"a": true, "b": true}); !ok || f != two {
		t.Errorf("f(a,b) should pick the two-arg overload")
	}
}

func TestStaticRegistryUnknownName(t *testing.T) {
	reg := NewStaticRegistry(nil)
	if _, ok := reg.Lookup("nope", nil); ok {
		t.Error("unknown name must miss")
	}
}

func TestStaticRegistryListStableOrder(t *testing.T) {
	reg := NewStaticRegistry([]*Function{
		{Name: "zed"},
		{Name: "alpha"},
		{Name: "alpha"},
	})
	got := reg.List()
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	if got[0].Name != "alpha" || got[1].Name != "alpha" || got[2].Name != "zed" {
		t.Errorf("List order = %v", []string{got[0].Name, got[1].Name, got[2].Name})
	}
}

func TestEmptyRegistry(t *testing.T) {
	var reg Registry = EmptyRegistry{}
	if _, ok := reg.Lookup("anything", ArgSet{"x": true}); ok {
		t.Error("EmptyRegistry must always miss")
	}
	if reg.List() != nil {
		t.Error("EmptyRegistry.List must be nil")
	}
}

// TestParseRegistryComment checks a declaration's comment field rides into the
// Function, where the OpenAPI generator reads it.
func TestParseRegistryComment(t *testing.T) {
	reg, err := ParseRegistry(`[{
		"name": "add",
		"sql": "select :a + :b",
		"comment": "Add two numbers\nReturns the sum.",
		"params": [{"name": "a", "type": "int4"}, {"name": "b", "type": "int4"}],
		"returns": {"kind": "scalar", "type": "int4"}
	}]`)
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	f, ok := reg.Lookup("add", ArgSet{"a": true, "b": true})
	if !ok {
		t.Fatal("add not found")
	}
	if f.Comment != "Add two numbers\nReturns the sum." {
		t.Errorf("Comment = %q", f.Comment)
	}
}
