package sqlite

import (
	"context"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
)

func execCall(t *testing.T, b *Backend, c *ir.Call, fn *rpc.Function) []map[string]any {
	t.Helper()
	plan := &ir.Plan{Call: c, Func: fn, ReadOnly: fn.Volatility.ReadOnly()}
	rc := &reqctx.Context{Role: "anon", Method: "POST"}
	res, err := b.Execute(context.Background(), plan, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return readAll(t, res)
}

func TestFunctionsDefaultsToEmpty(t *testing.T) {
	b := openSeeded(t)
	if _, ok := b.Functions().Lookup("x", nil); ok {
		t.Error("a backend with no registry must miss every lookup")
	}
}

func TestExecuteCallScalar(t *testing.T) {
	b := openSeeded(t)
	fn := &rpc.Function{
		Name:       "add_them",
		Params:     []rpc.Param{{Name: "a"}, {Name: "b"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: "SELECT :a + :b"},
	}
	rows := execCall(t, b, &ir.Call{Args: map[string]ir.Value{
		"a": {JSON: int64(2)}, "b": {JSON: int64(3)},
	}}, fn)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// One column, the value 5.
	for _, v := range rows[0] {
		if got, ok := v.(int64); !ok || got != 5 {
			t.Errorf("value = %#v, want 5", v)
		}
	}
}

func TestExecuteCallTableReturn(t *testing.T) {
	b := openSeeded(t)
	fn := &rpc.Function{
		Name:       "films_after",
		Params:     []rpc.Param{{Name: "y"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y ORDER BY id"},
	}
	rows := execCall(t, b, &ir.Call{Args: map[string]ir.Value{"y": {Text: "1950"}}}, fn)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["title"] != "Blade Runner" {
		t.Errorf("first title = %v", rows[0]["title"])
	}
}

func TestExecuteCallVolatilePersists(t *testing.T) {
	b := openSeeded(t)
	fn := &rpc.Function{
		Name:       "rate_film",
		Params:     []rpc.Param{{Name: "film_id"}, {Name: "r"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Volatility: rpc.Volatile,
		Query:      &rpc.PortableQuery{SQL: "UPDATE films SET rating = :r WHERE id = :film_id RETURNING rating"},
	}
	rows := execCall(t, b, &ir.Call{Args: map[string]ir.Value{
		"film_id": {JSON: int64(1)}, "r": {JSON: "G"},
	}}, fn)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	// The update committed.
	var got string
	if err := b.DB().QueryRow("SELECT rating FROM films WHERE id = 1").Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != "G" {
		t.Errorf("rating = %q, want G", got)
	}
}

func TestExecuteCallVolatileRollback(t *testing.T) {
	b := openSeeded(t)
	roll := ir.TxRollback
	fn := &rpc.Function{
		Name:       "rate_film",
		Params:     []rpc.Param{{Name: "film_id"}, {Name: "r"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar},
		Volatility: rpc.Volatile,
		Query:      &rpc.PortableQuery{SQL: "UPDATE films SET rating = :r WHERE id = :film_id RETURNING rating"},
	}
	c := &ir.Call{
		Args:   map[string]ir.Value{"film_id": {JSON: int64(2)}, "r": {JSON: "G"}},
		Prefer: ir.PreferSet{Tx: &roll},
	}
	execCall(t, b, c, fn)

	// The update rolled back; the original rating stands.
	var got string
	if err := b.DB().QueryRow("SELECT rating FROM films WHERE id = 2").Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != "R" {
		t.Errorf("rating = %q, want the unchanged R", got)
	}
}
