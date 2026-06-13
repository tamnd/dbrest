package httpapi

import (
	"io"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
)

// rowStream is a forward-only stub over fixed rows, enough to drive renderCall.
type rowStream struct {
	cols []string
	rows [][]any
	pos  int
}

func (s *rowStream) Columns() []string { return s.cols }
func (s *rowStream) Next() bool {
	if s.pos >= len(s.rows) {
		return false
	}
	s.pos++
	return true
}
func (s *rowStream) Values() ([]any, error) { return s.rows[s.pos-1], nil }
func (s *rowStream) Err() error             { return nil }
func (s *rowStream) Close() error           { return nil }

// rowResult is a backend.Result backed by an in-memory row stream.
type rowResult struct{ s *rowStream }

func (r rowResult) Body() io.Reader                 { return nil }
func (r rowResult) Rows() backend.RowStream         { return r.s }
func (r rowResult) Count() (int64, bool)            { return 0, false }
func (r rowResult) Affected() (int64, bool)         { return 0, false }
func (r rowResult) ResponseControls() *reqctx.ResponseControls {
	return &reqctx.ResponseControls{}
}

func resultOf(cols []string, rows ...[]any) backend.Result {
	return rowResult{s: &rowStream{cols: cols, rows: rows}}
}

// TestRenderCallShapes covers finding 03-P06: renderCall shapes a native RPC
// result by the function's introspected return kind, not by a column-name guess.
// A SETOF scalar is a JSON array of bare values (no longer truncated to the first
// row); a single composite is one bare object (no longer wrapped in an array); a
// table whose lone column collides with the function name is still an array of
// objects (no longer collapsed to a scalar); a scalar with a named OUT parameter
// is the bare value (no longer an object).
func TestRenderCallShapes(t *testing.T) {
	cases := []struct {
		name string
		fn   *rpc.Function
		res  backend.Result
		want string
	}{
		{
			name: "setof scalar is an array of bare values",
			fn:   &rpc.Function{Name: "ret_setof_integers", Returns: rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "int4"}},
			res:  resultOf([]string{"ret_setof_integers"}, []any{int64(1)}, []any{int64(2)}, []any{int64(3)}),
			want: "[1,2,3]",
		},
		{
			name: "single composite is one bare object",
			fn:   &rpc.Function{Name: "ret_point_2d", Returns: rpc.ReturnShape{Kind: rpc.ReturnObject}},
			res:  resultOf([]string{"x", "y"}, []any{int64(10), int64(5)}),
			want: `{"x":10,"y":5}`,
		},
		{
			name: "single composite with no row is null",
			fn:   &rpc.Function{Name: "ret_point_2d", Returns: rpc.ReturnShape{Kind: rpc.ReturnObject}},
			res:  resultOf([]string{"x", "y"}),
			want: "null",
		},
		{
			name: "name-collision table is an array of objects",
			fn:   &rpc.Function{Name: "title", Returns: rpc.ReturnShape{Kind: rpc.ReturnTable}},
			res:  resultOf([]string{"title"}, []any{"Dune"}, []any{"Arrival"}),
			want: `[{"title":"Dune"},{"title":"Arrival"}]`,
		},
		{
			name: "scalar with a named OUT parameter is the bare value",
			fn:   &rpc.Function{Name: "add", Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "int4"}},
			res:  resultOf([]string{"sum"}, []any{int64(7)}),
			want: "7",
		},
		{
			name: "plain scalar is the bare value",
			fn:   &rpc.Function{Name: "now_year", Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "int4"}},
			res:  resultOf([]string{"now_year"}, []any{int64(2026)}),
			want: "2026",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, apiErr := renderCall(mediaJSON, c.res, c.fn, c.fn.Name)
			if apiErr != nil {
				t.Fatalf("renderCall: %v", apiErr)
			}
			if string(out.body) != c.want {
				t.Errorf("body = %s, want %s", out.body, c.want)
			}
		})
	}
}

// A nil descriptor (a function the catalog never introspected) keeps the legacy
// column-name fallback: a lone column named after the function is a scalar, and
// anything wider renders like a table read. This guards the regression-safe path
// nativeFunc relies on when it returns nil.
func TestRenderCallNilFallback(t *testing.T) {
	scalar := resultOf([]string{"answer"}, []any{int64(42)})
	out, apiErr := renderCall(mediaJSON, scalar, nil, "answer")
	if apiErr != nil {
		t.Fatalf("renderCall(scalar fallback): %v", apiErr)
	}
	if string(out.body) != "42" {
		t.Errorf("scalar fallback body = %s, want 42", out.body)
	}

	table := resultOf([]string{"a", "b"}, []any{int64(1), int64(2)})
	out, apiErr = renderCall(mediaJSON, table, nil, "some_fn")
	if apiErr != nil {
		t.Fatalf("renderCall(table fallback): %v", apiErr)
	}
	if string(out.body) != `[{"a":1,"b":2}]` {
		t.Errorf("table fallback body = %s, want [{\"a\":1,\"b\":2}]", out.body)
	}
}
