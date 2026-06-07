package sqlgen

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/rpc"
)

func compileCall(t *testing.T, c *ir.Call, fn *rpc.Function) *Statement {
	t.Helper()
	st, err := CompileCall(stub{}, c, fn)
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
	_, err := CompileCall(stub{}, &ir.Call{}, fn)
	if err == nil || err.Code != "PGRST127" {
		t.Fatalf("want PGRST127, got %v", err)
	}
}
