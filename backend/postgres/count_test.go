package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/ir"
)

// These count-strategy tests reach the unexported computeCount/parseExplainRows,
// so they live in the internal package and carry their own DSN gate rather than
// borrowing the external integration helpers.
func countDSN(t *testing.T) string {
	t.Helper()
	s := os.Getenv("DBREST_PG_DSN")
	if s == "" {
		t.Skip("DBREST_PG_DSN not set; skipping postgres count integration tests")
	}
	return s
}

func openCount(t *testing.T, dsn string) *Backend {
	t.Helper()
	be, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	be.SetSchemas([]string{"public"})
	return be
}

func mustExec(t *testing.T, b *Backend, sql string) {
	t.Helper()
	if _, err := b.Pool().Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func beginTx(t *testing.T, b *Backend) pgx.Tx {
	t.Helper()
	tx, err := b.Pool().Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

// parseExplainRows reads the root node's row estimate out of the documented
// EXPLAIN (FORMAT JSON) shape, rounding the planner's fractional estimate.
func TestParseExplainRows(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "seq scan estimate",
			raw:  `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"films","Plan Rows":1234,"Plan Width":8}}]`,
			want: 1234,
		},
		{
			name: "fractional estimate rounds",
			raw:  `[{"Plan":{"Node Type":"Index Scan","Plan Rows":41.6}}]`,
			want: 42,
		},
		{
			name: "rounds down below half",
			raw:  `[{"Plan":{"Node Type":"Index Scan","Plan Rows":41.2}}]`,
			want: 41,
		},
		{
			name: "nested child does not shadow the root estimate",
			raw: `[{"Plan":{"Node Type":"Aggregate","Plan Rows":1,` +
				`"Plans":[{"Node Type":"Seq Scan","Plan Rows":9999}]}}]`,
			want: 1,
		},
		{
			name: "zero rows",
			raw:  `[{"Plan":{"Node Type":"Result","Plan Rows":0}}]`,
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseExplainRows([]byte(c.raw))
			if err != nil {
				t.Fatalf("parseExplainRows: %v", err)
			}
			if got != c.want {
				t.Errorf("rows = %d, want %d", got, c.want)
			}
		})
	}
}

func TestParseExplainRowsRejectsGarbage(t *testing.T) {
	if _, err := parseExplainRows([]byte("not json")); err == nil {
		t.Error("want error on non-JSON EXPLAIN output")
	}
	if _, err := parseExplainRows([]byte("[]")); err == nil {
		t.Error("want error on an empty plan array")
	}
}

// The estimated count is exact when no db-max-rows threshold is configured: with
// nothing to cross over at, the planner estimate never enters in.
func TestEstimatedCountExactWithoutThreshold(t *testing.T) {
	dsn := countDSN(t) // skips without DBREST_PG_DSN
	b := openCount(t, dsn)
	defer b.Close()

	mustExec(t, b, `DROP TABLE IF EXISTS estc; CREATE TABLE estc(id int);
		INSERT INTO estc SELECT g FROM generate_series(1, 30) g;`)

	tx := beginTx(t, b)
	defer tx.Rollback(context.Background())
	q := &ir.Query{Relation: ir.Ref{Schema: "public", Name: "estc"}, Count: ir.CountEstimated}
	got, err := b.computeCount(context.Background(), tx, q)
	if err != nil {
		t.Fatalf("computeCount: %v", err)
	}
	if got != 30 {
		t.Errorf("estimated count without threshold = %d, want exact 30", got)
	}
}

// Below the threshold an estimated count is exact; the capped probe returns the
// true total without ever consulting the planner.
func TestEstimatedCountExactBelowThreshold(t *testing.T) {
	dsn := countDSN(t)
	b := openCount(t, dsn)
	defer b.Close()

	mustExec(t, b, `DROP TABLE IF EXISTS estc; CREATE TABLE estc(id int);
		INSERT INTO estc SELECT g FROM generate_series(1, 30) g;`)

	tx := beginTx(t, b)
	defer tx.Rollback(context.Background())
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "estc"},
		Count:    ir.CountEstimated,
		CountMax: 100,
	}
	got, err := b.computeCount(context.Background(), tx, q)
	if err != nil {
		t.Fatalf("computeCount: %v", err)
	}
	if got != 30 {
		t.Errorf("estimated count below threshold = %d, want exact 30", got)
	}
}

// A planned count returns the planner estimate, which after ANALYZE matches the
// real row count closely for a simple table.
func TestPlannedCountUsesPlannerEstimate(t *testing.T) {
	dsn := countDSN(t)
	b := openCount(t, dsn)
	defer b.Close()

	mustExec(t, b, `DROP TABLE IF EXISTS estc; CREATE TABLE estc(id int);
		INSERT INTO estc SELECT g FROM generate_series(1, 500) g; ANALYZE estc;`)

	tx := beginTx(t, b)
	defer tx.Rollback(context.Background())
	q := &ir.Query{Relation: ir.Ref{Schema: "public", Name: "estc"}, Count: ir.CountPlanned}
	got, err := b.computeCount(context.Background(), tx, q)
	if err != nil {
		t.Fatalf("computeCount: %v", err)
	}
	// The planner estimate of a freshly analyzed 500-row table is exact here.
	if got != 500 {
		t.Errorf("planned count = %d, want the planner estimate 500", got)
	}
}
