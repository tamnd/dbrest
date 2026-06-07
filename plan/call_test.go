package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/rpc"
)

func reg(fns ...*rpc.Function) rpc.Registry {
	return rpc.NewStaticRegistry(fns)
}

func addThem() *rpc.Function {
	return &rpc.Function{
		Name:       "add_them",
		Params:     []rpc.Param{{Name: "a", Type: "integer"}, {Name: "b", Type: "integer"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "integer"},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: "SELECT :a + :b"},
	}
}

func TestCallResolvesFunction(t *testing.T) {
	c := &ir.Call{Function: ir.Ref{Name: "add_them"}, Args: map[string]ir.Value{
		"a": {Text: "2"}, "b": {Text: "3"},
	}}
	p, err := Call(reg(addThem()), c, true, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if p.Func == nil || p.Func.Name != "add_them" {
		t.Fatalf("function not bound: %+v", p.Func)
	}
	if !p.ReadOnly {
		t.Error("an immutable function plans read-only")
	}
}

func TestCallNoFunctionIs404(t *testing.T) {
	c := &ir.Call{Function: ir.Ref{Name: "nope"}}
	_, err := Call(reg(addThem()), c, true, nil)
	if err == nil || err.Code != "PGRST202" {
		t.Fatalf("want PGRST202, got %v", err)
	}
}

func TestCallArgMismatchIs404(t *testing.T) {
	// add_them needs a and b; only a is supplied.
	c := &ir.Call{Function: ir.Ref{Name: "add_them"}, Args: map[string]ir.Value{"a": {Text: "2"}}}
	_, err := Call(reg(addThem()), c, true, nil)
	if err == nil || err.Code != "PGRST202" {
		t.Fatalf("want PGRST202, got %v", err)
	}
}

func TestCallGetOnVolatileIs405(t *testing.T) {
	vol := &rpc.Function{
		Name:       "do_thing",
		Volatility: rpc.Volatile,
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:      &rpc.PortableQuery{SQL: "SELECT 1"},
	}
	c := &ir.Call{Function: ir.Ref{Name: "do_thing"}}
	_, err := Call(reg(vol), c, true, nil)
	if err == nil || err.Code != "PGRST101" {
		t.Fatalf("want PGRST101, got %v", err)
	}
	if err.HTTPStatus != 405 {
		t.Errorf("status = %d, want 405", err.HTTPStatus)
	}
}

func TestCallPostOnVolatileIsAllowed(t *testing.T) {
	vol := &rpc.Function{
		Name:       "do_thing",
		Volatility: rpc.Volatile,
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:      &rpc.PortableQuery{SQL: "SELECT 1"},
	}
	c := &ir.Call{Function: ir.Ref{Name: "do_thing"}}
	p, err := Call(reg(vol), c, false, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if p.ReadOnly {
		t.Error("a volatile function plans read-write")
	}
}

func TestCallPostFilterUnknownColumn(t *testing.T) {
	tab := &rpc.Function{
		Name:       "films_after",
		Params:     []rpc.Param{{Name: "y", Type: "integer"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y"},
	}
	c := &ir.Call{
		Function: ir.Ref{Name: "films_after"},
		Args:     map[string]ir.Value{"y": {Text: "2000"}},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"bogus"}}},
	}
	_, err := Call(reg(tab), c, true, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

func TestCallPostFilterKnownColumnOK(t *testing.T) {
	tab := &rpc.Function{
		Name:       "films_after",
		Params:     []rpc.Param{{Name: "y", Type: "integer"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y"},
	}
	c := &ir.Call{
		Function: ir.Ref{Name: "films_after"},
		Args:     map[string]ir.Value{"y": {Text: "2000"}},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"title"}}},
	}
	if _, err := Call(reg(tab), c, true, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
}
