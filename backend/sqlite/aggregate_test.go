package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/plan"
)

// openSales seeds a sales table with a category and an amount so an aggregate
// has something to fold over.
func openSales(t *testing.T) *Backend {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	_, err = b.DB().Exec(`
		CREATE TABLE sales (id INTEGER PRIMARY KEY, category TEXT NOT NULL, amount INTEGER NOT NULL);
		INSERT INTO sales (id, category, amount) VALUES
			(1, 'a', 10), (2, 'a', 20), (3, 'b', 5);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return b
}

// planAgg parses and plans a sales read with aggregates enabled.
func planAgg(t *testing.T, b *Backend, query string) *ir.Query {
	t.Helper()
	q, perr := ir.ParseRead("sales", query, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	pl, perr := plan.Read(model, q, nil, plan.Options{AggregatesEnabled: true})
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	return pl.Query
}

func TestExecuteBareCount(t *testing.T) {
	b := openSales(t)
	q := planAgg(t, b, "select=count()")
	rows := execRead(t, b, q)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := fmt.Sprint(rows[0]["count"]); got != "3" {
		t.Errorf("count = %v, want 3", rows[0]["count"])
	}
}

func TestExecuteGroupedSum(t *testing.T) {
	b := openSales(t)
	q := planAgg(t, b, "select=category,amount.sum()&order=category")
	rows := execRead(t, b, q)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one per category)", len(rows))
	}
	got := map[string]string{}
	for _, r := range rows {
		cat, _ := asString(r["category"])
		got[cat] = fmt.Sprint(r["sum"])
	}
	if got["a"] != "30" || got["b"] != "5" {
		t.Errorf("sums = %v, want a:30 b:5", got)
	}
}

func TestExecuteAggregateWithAlias(t *testing.T) {
	b := openSales(t)
	q := planAgg(t, b, "select=category,total:amount.sum()&order=category")
	rows := execRead(t, b, q)
	if _, ok := rows[0]["total"]; !ok {
		t.Fatalf("expected a 'total' key, got %v", rows[0])
	}
}

// TestCompileGroupedSumSQL pins the GROUP BY shape the grouped aggregate lowers
// to on SQLite.
func TestCompileGroupedSumSQL(t *testing.T) {
	b := openSales(t)
	q := planAgg(t, b, "select=category,amount.sum()")
	st, perr := sqlgen.CompileRead(dialect{}, q)
	if perr != nil {
		t.Fatalf("CompileRead: %v", perr)
	}
	want := `SELECT "category", sum("amount") AS "sum" FROM "sales" GROUP BY "category"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}
