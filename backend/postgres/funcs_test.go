package postgres

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/rpc"
)

// call builds a minimal ir.Call naming a function, for the native-resolution tests.
func call(name string) *ir.Call { return &ir.Call{Function: ir.Ref{Name: name}} }

// TestReturnShapeFor covers finding 03-P06: the native RPC return shape is taken
// from pg_proc facts (proretset and the return type's class), not guessed from
// column names. A composite or record return is object-shaped; everything else is
// scalar-shaped; proretset then decides array vs single.
func TestReturnShapeFor(t *testing.T) {
	cases := []struct {
		name    string
		retset  bool
		rettype uint32
		typtype string
		typname string
		want    rpc.ReturnKind
	}{
		{"scalar integer", false, 23, "b", "int4", rpc.ReturnScalar},
		{"setof integer", true, 23, "b", "int4", rpc.ReturnSetOf},
		{"single composite", false, 16385, "c", "point_2d", rpc.ReturnObject},
		{"setof composite", true, 16385, "c", "point_2d", rpc.ReturnTable},
		{"returns table", true, oidRecord, "p", "record", rpc.ReturnTable},
		{"returns record single", false, oidRecord, "p", "record", rpc.ReturnObject},
		{"returns void", false, oidVoid, "p", "void", rpc.ReturnVoid},
		{"setof void stays void", true, oidVoid, "p", "void", rpc.ReturnVoid},
		{"scalar enum", false, 16400, "e", "mood", rpc.ReturnScalar},
		{"scalar json", false, 114, "b", "json", rpc.ReturnScalar},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := returnShapeFor(c.retset, c.rettype, c.typtype, c.typname)
			if got.Kind != c.want {
				t.Errorf("returnShapeFor(%v,%d,%q) Kind = %v, want %v", c.retset, c.rettype, c.typtype, got.Kind, c.want)
			}
		})
	}
}

// A scalar or setof-scalar shape carries the return type name so the renderer can
// embed a json/jsonb value verbatim; an object/table/void shape needs no Type.
func TestReturnShapeForType(t *testing.T) {
	if got := returnShapeFor(false, 114, "b", "json"); got.Type != "json" {
		t.Errorf("scalar Type = %q, want json", got.Type)
	}
	if got := returnShapeFor(true, 23, "b", "int4"); got.Type != "int4" {
		t.Errorf("setof Type = %q, want int4", got.Type)
	}
	if got := returnShapeFor(false, 16385, "c", "point_2d"); got.Type != "" {
		t.Errorf("object Type = %q, want empty", got.Type)
	}
}

// nativeFunc returns nil when the catalog has no entry for the call, so the
// renderer keeps its column-name fallback rather than asserting a wrong shape.
func TestNativeFuncUnknown(t *testing.T) {
	b := &Backend{funcRet: map[string]rpc.ReturnShape{}}
	if got := b.nativeFunc(call("missing"), "public"); got != nil {
		t.Errorf("nativeFunc(missing) = %v, want nil", got)
	}
	b2 := &Backend{} // funcRet nil (never introspected)
	if got := b2.nativeFunc(call("anything"), "public"); got != nil {
		t.Errorf("nativeFunc with nil funcRet = %v, want nil", got)
	}
}

// nativeFunc builds a native descriptor (Query nil, so portableCall is false)
// carrying the introspected return shape and volatility.
func TestNativeFuncResolved(t *testing.T) {
	b := &Backend{
		funcRet: map[string]rpc.ReturnShape{"public.ret_point": {Kind: rpc.ReturnObject}},
		funcVol: map[string]rpc.Volatility{"public.ret_point": rpc.Stable},
	}
	fn := b.nativeFunc(call("ret_point"), "public")
	if fn == nil {
		t.Fatal("nativeFunc(ret_point) = nil, want descriptor")
	}
	if fn.Returns.Kind != rpc.ReturnObject {
		t.Errorf("Returns.Kind = %v, want ReturnObject", fn.Returns.Kind)
	}
	if fn.Volatility != rpc.Stable {
		t.Errorf("Volatility = %v, want Stable", fn.Volatility)
	}
	if fn.Query != nil {
		t.Error("native descriptor must leave Query nil so it lowers through the splice path")
	}
}
