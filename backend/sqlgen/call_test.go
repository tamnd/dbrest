package sqlgen

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/rpc"
)

func compileCall(t *testing.T, c *ir.Call, fn *rpc.Function) *Statement {
	t.Helper()
	st, err := CompileCall(stub{}, c, fn, nil)
	if err != nil {
		t.Fatalf("CompileCall: %v", err)
	}
	return st
}

func TestCompileCallScalarBindsArgs(t *testing.T) {
	fn := &rpc.Function{
		Name:    "add_them",
		Params:  []rpc.Param{{Name: "a"}, {Name: "b"}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT :a + :b"},
	}
	c := &ir.Call{Args: map[string]ir.Value{"a": {Text: "2"}, "b": {Text: "3"}}}
	st := compileCall(t, c, fn)
	if st.SQL != "SELECT $1 + $2" {
		t.Errorf("SQL = %q, want SELECT $1 + $2", st.SQL)
	}
	if len(st.Args) != 2 || st.Args[0] != "2" || st.Args[1] != "3" {
		t.Errorf("Args = %v, want [2 3]", st.Args)
	}
}

func TestCompileCallCastGuard(t *testing.T) {
	// The :: cast must not be read as a placeholder.
	fn := &rpc.Function{
		Name:    "f",
		Params:  []rpc.Param{{Name: "a"}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT :a::text"},
	}
	c := &ir.Call{Args: map[string]ir.Value{"a": {Text: "x"}}}
	st := compileCall(t, c, fn)
	if st.SQL != "SELECT $1::text" {
		t.Errorf("SQL = %q, want SELECT $1::text", st.SQL)
	}
}

func TestCompileCallOptionalDefault(t *testing.T) {
	fn := &rpc.Function{
		Name:    "g",
		Params:  []rpc.Param{{Name: "a"}, {Name: "b", Optional: true, Default: int64(10)}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT :a + :b"},
	}
	c := &ir.Call{Args: map[string]ir.Value{"a": {Text: "1"}}}
	st := compileCall(t, c, fn)
	if len(st.Args) != 2 || st.Args[1] != int64(10) {
		t.Errorf("Args = %v, want second to be the default 10", st.Args)
	}
}

// A variadic parameter expands its placeholder into one bound value per element,
// so an IN (:ids) clause binds every collected id. The GET list form is exercised
// here; the POST array form lands the same elements through the JSON path.
func TestCompileCallVariadicExpandsList(t *testing.T) {
	fn := &rpc.Function{
		Name:    "pick",
		Params:  []rpc.Param{{Name: "ids", Variadic: true}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnSetOf},
		Query:   &rpc.PortableQuery{SQL: "SELECT title FROM films WHERE id IN (:ids)"},
	}
	c := &ir.Call{Args: map[string]ir.Value{"ids": {List: []string{"1", "3"}}}}
	st := compileCall(t, c, fn)
	if st.SQL != "SELECT title FROM films WHERE id IN ($1, $2)" {
		t.Errorf("SQL = %q", st.SQL)
	}
	if len(st.Args) != 2 || st.Args[0] != "1" || st.Args[1] != "3" {
		t.Errorf("Args = %v, want [1 3]", st.Args)
	}
}

// A variadic call with no trailing arguments expands to nothing, so f(:ids)
// becomes f() and binds no values.
func TestCompileCallVariadicEmpty(t *testing.T) {
	fn := &rpc.Function{
		Name:    "pick",
		Params:  []rpc.Param{{Name: "ids", Variadic: true}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnSetOf},
		Query:   &rpc.PortableQuery{SQL: "SELECT count_ids(:ids)"},
	}
	st := compileCall(t, &ir.Call{Args: map[string]ir.Value{}}, fn)
	if st.SQL != "SELECT count_ids()" {
		t.Errorf("SQL = %q, want SELECT count_ids()", st.SQL)
	}
	if len(st.Args) != 0 {
		t.Errorf("Args = %v, want none", st.Args)
	}
}

func TestCompileCallTableWithPostFilter(t *testing.T) {
	fn := &rpc.Function{
		Name:    "films_after",
		Params:  []rpc.Param{{Name: "y"}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
		Query:   &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y"},
	}
	limit := 5
	c := &ir.Call{
		Args:   map[string]ir.Value{"y": {Text: "2000"}},
		Select: []ir.SelectItem{col("title")},
		Limit:  &limit,
	}
	st := compileCall(t, c, fn)
	want := `SELECT "title" FROM (SELECT id, title FROM films WHERE year > $1) _rpc LIMIT 5`
	if st.SQL != want {
		t.Errorf("SQL = %q\nwant   %q", st.SQL, want)
	}
}

func TestCompileCallTableNoPostFilterIsVerbatim(t *testing.T) {
	fn := &rpc.Function{
		Name:    "all_films",
		Returns: rpc.ReturnShape{Kind: rpc.ReturnTable},
		Query:   &rpc.PortableQuery{SQL: "SELECT id, title FROM films"},
	}
	st := compileCall(t, &ir.Call{}, fn)
	if st.SQL != "SELECT id, title FROM films" {
		t.Errorf("SQL = %q", st.SQL)
	}
}

func TestCompileCallSingleObjectArg(t *testing.T) {
	// One json parameter receives the whole posted object.
	fn := &rpc.Function{
		Name:    "ins",
		Params:  []rpc.Param{{Name: "payload", Type: "json"}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT json_extract(:payload, '$.title')"},
	}
	c := &ir.Call{Args: map[string]ir.Value{
		"title": {JSON: "Dune"},
		"year":  {JSON: float64(2021)},
	}}
	st := compileCall(t, c, fn)
	if len(st.Args) != 1 {
		t.Fatalf("Args = %v, want a single bound object", st.Args)
	}
	if s, ok := st.Args[0].(string); !ok || s == "" {
		t.Errorf("bound arg = %#v, want the object re-encoded as JSON text", st.Args[0])
	}
}

func TestCompileCallNoRealizationUnsupported(t *testing.T) {
	fn := &rpc.Function{Name: "native_only", Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar}}
	_, err := CompileCall(stub{}, &ir.Call{}, fn, nil)
	if err == nil || err.Code != "PGRST127" {
		t.Fatalf("want PGRST127, got %v", err)
	}
}

// The count of a table-returning function wraps the body in count(*) and applies
// the post-filter, leaving select, order, and window out of the count, exactly
// as a table read's count does.
func TestCompileCallCountWrapsAndFilters(t *testing.T) {
	fn := &rpc.Function{
		Name:    "films_after",
		Params:  []rpc.Param{{Name: "y"}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}}},
		Query:   &rpc.PortableQuery{SQL: "SELECT id FROM films WHERE year > :y"},
	}
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpGt, Value: ir.Value{Text: "10"}})
	c := &ir.Call{Args: map[string]ir.Value{"y": {Text: "2000"}}, Where: &where}
	st, err := CompileCallCount(stub{}, c, fn, nil)
	if err != nil {
		t.Fatalf("CompileCallCount: %v", err)
	}
	want := `SELECT count(*) FROM (SELECT id FROM films WHERE year > $1) _rpc WHERE "id" > $2`
	if st.SQL != want {
		t.Errorf("SQL = %q\nwant   %q", st.SQL, want)
	}
	if len(st.Args) != 2 || st.Args[0] != "2000" || st.Args[1] != "10" {
		t.Errorf("Args = %v, want [2000 10]", st.Args)
	}
}

// A function with no portable realization cannot be counted either; it reports
// PGRST127 rather than running an empty body.
func TestCompileCallCountNoRealizationUnsupported(t *testing.T) {
	fn := &rpc.Function{Name: "native_only", Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar}}
	_, err := CompileCallCount(stub{}, &ir.Call{}, fn, nil)
	if err == nil || err.Code != "PGRST127" {
		t.Fatalf("want PGRST127, got %v", err)
	}
}

// A nil function (a misrouted native call) reports an error instead of
// dereferencing the nil pointer, the regression behind the count=exact crash.
func TestCompileCallCountNilFunctionErrors(t *testing.T) {
	_, err := CompileCallCount(stub{}, &ir.Call{}, nil, nil)
	if err == nil {
		t.Fatal("want an error for a nil function, got nil")
	}
}

func TestCompileCallNilFunctionErrors(t *testing.T) {
	_, err := CompileCall(stub{}, &ir.Call{}, nil, nil)
	if err == nil {
		t.Fatal("want an error for a nil function, got nil")
	}
}

// A placeholder that is not a declared parameter binds the reserved request-
// context value, the emulated analog of current_setting('request.method').
func TestCompileCallContextPlaceholder(t *testing.T) {
	fn := &rpc.Function{
		Name:    "get_request_method",
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT :request_method"},
	}
	st, err := CompileCall(stub{}, &ir.Call{}, fn, map[string]any{"request_method": "GET"})
	if err != nil {
		t.Fatalf("CompileCall: %v", err)
	}
	if st.SQL != "SELECT $1" {
		t.Errorf("SQL = %q, want SELECT $1", st.SQL)
	}
	if len(st.Args) != 1 || st.Args[0] != "GET" {
		t.Errorf("Args = %v, want [GET]", st.Args)
	}
}

// A declared parameter of the same name keeps winning over the context value.
func TestCompileCallDeclaredParamBeatsContext(t *testing.T) {
	fn := &rpc.Function{
		Name:    "f",
		Params:  []rpc.Param{{Name: "request_method"}},
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT :request_method"},
	}
	c := &ir.Call{Args: map[string]ir.Value{"request_method": {Text: "caller"}}}
	st, err := CompileCall(stub{}, c, fn, map[string]any{"request_method": "GET"})
	if err != nil {
		t.Fatalf("CompileCall: %v", err)
	}
	if len(st.Args) != 1 || st.Args[0] != "caller" {
		t.Errorf("Args = %v, want [caller]", st.Args)
	}
}

// Without context values an undeclared placeholder is still an internal error.
func TestCompileCallUndeclaredPlaceholderRejected(t *testing.T) {
	fn := &rpc.Function{
		Name:    "f",
		Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Query:   &rpc.PortableQuery{SQL: "SELECT :nope"},
	}
	if _, err := CompileCall(stub{}, &ir.Call{}, fn, nil); err == nil {
		t.Fatal("want error for undeclared placeholder")
	}
}
