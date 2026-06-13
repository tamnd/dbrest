package plan

import (
	"strings"
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
	p, err := Call(reg(addThem()), nil, c, true, nil)
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
	_, err := Call(reg(addThem()), nil, c, true, nil)
	if err == nil || err.Code != "PGRST202" {
		t.Fatalf("want PGRST202, got %v", err)
	}
}

func TestCallArgMismatchIs404(t *testing.T) {
	// add_them needs a and b; only a is supplied.
	c := &ir.Call{Function: ir.Ref{Name: "add_them"}, Args: map[string]ir.Value{"a": {Text: "2"}}}
	_, err := Call(reg(addThem()), nil, c, true, nil)
	if err == nil || err.Code != "PGRST202" {
		t.Fatalf("want PGRST202, got %v", err)
	}
}

// TestCallAmbiguousOverloadIs300 checks that two overloads tying at the top score
// surface as PGRST203 (a 300) carrying both competing signatures, rather than the
// planner silently picking one. Two single-optional-parameter overloads called
// with no arguments are equally good.
func TestCallAmbiguousOverloadIs300(t *testing.T) {
	left := &rpc.Function{
		Name:       "f",
		Params:     []rpc.Param{{Name: "a", Type: "integer", Optional: true}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: "SELECT 1"},
	}
	right := &rpc.Function{
		Name:       "f",
		Params:     []rpc.Param{{Name: "b", Type: "integer", Optional: true}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: "SELECT 1"},
	}
	c := &ir.Call{Function: ir.Ref{Name: "f"}}
	_, err := Call(reg(left, right), nil, c, true, nil)
	if err == nil || err.Code != "PGRST203" {
		t.Fatalf("want PGRST203, got %v", err)
	}
	if err.HTTPStatus != 300 {
		t.Errorf("status = %d, want 300", err.HTTPStatus)
	}
}

// TestCallNoFunctionMessageQualifiedWithHint checks the PGRST202 message names the
// function schema-qualified with the searched argument list, and that an overload
// of the same name rides along as the nearest-signature hint.
func TestCallNoFunctionMessageQualifiedWithHint(t *testing.T) {
	// add_them(a, b) exists; the call supplies (a, c), matching neither overload.
	c := &ir.Call{Function: ir.Ref{Name: "add_them"}, Args: map[string]ir.Value{
		"a": {Text: "1"}, "c": {Text: "2"},
	}}
	_, err := Call(reg(addThem()), nil, c, true, []string{"api"})
	if err == nil || err.Code != "PGRST202" {
		t.Fatalf("want PGRST202, got %v", err)
	}
	if want := "api.add_them(a, c)"; !strings.Contains(err.Message, want) {
		t.Errorf("message = %q, want it to mention %q", err.Message, want)
	}
	if err.Hint == nil {
		t.Fatal("PGRST202 should carry a nearest-signature hint")
	}
	if want := "add_them(a => integer, b => integer)"; !strings.Contains(*err.Hint, want) {
		t.Errorf("hint = %q, want it to mention %q", *err.Hint, want)
	}
}

// TestCallNoParameterlessMessage checks the "without parameters" phrasing when the
// call names a function with no arguments and none is registered.
func TestCallNoParameterlessMessage(t *testing.T) {
	c := &ir.Call{Function: ir.Ref{Name: "ghost"}}
	_, err := Call(reg(addThem()), nil, c, true, []string{"api"})
	if err == nil || err.Code != "PGRST202" {
		t.Fatalf("want PGRST202, got %v", err)
	}
	if want := "api.ghost without parameters"; !strings.Contains(err.Message, want) {
		t.Errorf("message = %q, want it to mention %q", err.Message, want)
	}
}

// TestCallGetPartitionsArgsFromFilters checks the GET argument-versus-filter
// split: a key naming a declared parameter binds as an argument, while a key that
// does not name a parameter is re-read as a post-filter on the table return. The
// function still resolves, and the filter lands in the call's WHERE.
func TestCallGetPartitionsArgsFromFilters(t *testing.T) {
	c := &ir.Call{
		Function: ir.Ref{Name: "films_after"},
		Args:     map[string]ir.Value{"y": {Text: "2000"}, "title": {Text: "eq.Arrival"}},
		RawGet: map[string][]string{
			"y":     {"2000"},
			"title": {"eq.Arrival"},
		},
	}
	p, err := Call(reg(filmsAfter()), nil, c, true, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if p.Func == nil || p.Func.Name != "films_after" {
		t.Fatalf("function not bound: %+v", p.Func)
	}
	// y stayed an argument; title moved out of the argument map.
	if _, ok := c.Args["y"]; !ok {
		t.Error("declared parameter y should remain an argument")
	}
	if _, ok := c.Args["title"]; ok {
		t.Error("title names no parameter and should not be an argument")
	}
	// title became a post-filter in the WHERE tree.
	if c.Where == nil {
		t.Fatal("the non-parameter key should have become a filter")
	}
	cmp, ok := (*c.Where).(ir.Compare)
	if !ok || len(cmp.Path) != 1 || cmp.Path[0] != "title" || cmp.Op != ir.OpEq {
		t.Errorf("WHERE = %#v, want title eq filter", *c.Where)
	}
}

// TestCallGetFilterUnknownColumnRejected checks a partitioned filter is still
// validated against the table return's declared columns, so a non-parameter key
// naming no column reaches PostgreSQL as 42703 (item 04.5) rather than silently
// dropped.
func TestCallGetFilterUnknownColumnRejected(t *testing.T) {
	c := &ir.Call{
		Function: ir.Ref{Name: "films_after"},
		Args:     map[string]ir.Value{"y": {Text: "2000"}, "ghost": {Text: "eq.1"}},
		RawGet: map[string][]string{
			"y":     {"2000"},
			"ghost": {"eq.1"},
		},
	}
	_, err := Call(reg(filmsAfter()), nil, c, true, nil)
	if err == nil || err.Code != "42703" {
		t.Fatalf("want 42703, got %v", err)
	}
}

// TestCallGetArgTypeCoercion checks a GET text argument is validated against its
// declared parameter type, so a non-integer value for an integer parameter is the
// same 22P02 a read filter raises, on every backend.
func TestCallGetArgTypeCoercion(t *testing.T) {
	c := &ir.Call{
		Function: ir.Ref{Name: "add_them"},
		Args:     map[string]ir.Value{"a": {Text: "notanint"}, "b": {Text: "3"}},
		RawGet:   map[string][]string{"a": {"notanint"}, "b": {"3"}},
	}
	_, err := Call(reg(addThem()), nil, c, true, nil)
	if err == nil || err.HTTPStatus != 400 {
		t.Fatalf("want a 400 coercion error, got %v", err)
	}
}

// A GET reaching a volatile function fails the way PostgREST's does: the read-only
// transaction rejects the write with SQLSTATE 25006 at 405, not a PGRST101. The
// registry path raises it from the declared volatility since it cannot run the
// call, but the code and status a client sees match the native path (item 04.6).
func TestCallGetOnVolatileIs405(t *testing.T) {
	vol := &rpc.Function{
		Name:       "do_thing",
		Volatility: rpc.Volatile,
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:      &rpc.PortableQuery{SQL: "SELECT 1"},
	}
	c := &ir.Call{Function: ir.Ref{Name: "do_thing"}}
	_, err := Call(reg(vol), nil, c, true, nil)
	if err == nil || err.Code != "25006" {
		t.Fatalf("want 25006, got %v", err)
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
	p, err := Call(reg(vol), nil, c, false, nil)
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
	_, err := Call(reg(tab), nil, c, true, nil)
	if err == nil || err.Code != "42703" {
		t.Fatalf("want 42703, got %v", err)
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
	if _, err := Call(reg(tab), nil, c, true, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
}

// filmsAfter is the table-returning fixture whose declared columns (id, title)
// the post-filter validators check against.
func filmsAfter() *rpc.Function {
	return &rpc.Function{
		Name:       "films_after",
		Params:     []rpc.Param{{Name: "y", Type: "integer"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y"},
	}
}

func callWith(where ir.Cond) *ir.Call {
	w := where
	return &ir.Call{
		Function: ir.Ref{Name: "films_after"},
		Args:     map[string]ir.Value{"y": {Text: "2000"}},
		Where:    &w,
	}
}

// A post-filter tree over the declared columns validates through every logical
// node: And, Or, Not, and the Compare leaves all reference known columns.
func TestCallPostFilterWhereTreeKnownColumns(t *testing.T) {
	where := ir.Cond(ir.And{Kids: []ir.Cond{
		ir.Compare{Path: []string{"id"}, Op: ir.OpGt, Value: ir.Value{Text: "10"}},
		ir.Or{Kids: []ir.Cond{
			ir.Compare{Path: []string{"title"}, Op: ir.OpLike, Value: ir.Value{Text: "A*"}},
			ir.Not{Kid: ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "0"}}},
		}},
	}})
	if _, err := Call(reg(filmsAfter()), nil, callWith(where), true, nil); err != nil {
		t.Fatalf("Call with a valid filter tree: %v", err)
	}
}

// An unknown column anywhere in the tree is rejected, including one buried under
// Or and Not, so the validator recurses rather than checking only the top node.
func TestCallPostFilterWhereTreeUnknownColumn(t *testing.T) {
	cases := map[string]ir.Cond{
		"top-compare": ir.Compare{Path: []string{"ghost"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}},
		"under-and": ir.And{Kids: []ir.Cond{
			ir.Compare{Path: []string{"id"}, Op: ir.OpGt, Value: ir.Value{Text: "1"}},
			ir.Compare{Path: []string{"ghost"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}},
		}},
		"under-or-not": ir.Or{Kids: []ir.Cond{
			ir.Not{Kid: ir.Compare{Path: []string{"ghost"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}}},
		}},
	}
	for name, where := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Call(reg(filmsAfter()), nil, callWith(where), true, nil)
			if err == nil || err.Code != "42703" {
				t.Fatalf("want 42703, got %v", err)
			}
		})
	}
}

// An order term naming a column the table return does not declare is rejected.
func TestCallPostFilterOrderUnknownColumn(t *testing.T) {
	c := &ir.Call{
		Function: ir.Ref{Name: "films_after"},
		Args:     map[string]ir.Value{"y": {Text: "2000"}},
		Order:    []ir.OrderTerm{{Path: []string{"ghost"}}},
	}
	_, err := Call(reg(filmsAfter()), nil, c, true, nil)
	if err == nil || err.Code != "42703" {
		t.Fatalf("want 42703, got %v", err)
	}
}

// A scalar return declares no columns, so its post-filters are not validated
// here (they are checked against the engine result at run time).
func TestCallScalarReturnSkipsFilterValidation(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"anything"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}})
	c := &ir.Call{
		Function: ir.Ref{Name: "add_them"},
		Args:     map[string]ir.Value{"a": {Text: "1"}, "b": {Text: "2"}},
		Where:    &where,
	}
	if _, err := Call(reg(addThem()), nil, c, true, nil); err != nil {
		t.Fatalf("scalar return should not validate post-filter columns: %v", err)
	}
}
