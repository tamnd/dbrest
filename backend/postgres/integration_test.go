package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/tamnd/dbrest/backend/postgres"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
)

// dsn returns the DSN for the integration tests. The tests are skipped entirely
// when DBREST_PG_DSN is not set so the test suite passes without a live server.
func dsn(t *testing.T) string {
	t.Helper()
	s := os.Getenv("DBREST_PG_DSN")
	if s == "" {
		t.Skip("DBREST_PG_DSN not set; skipping postgres integration tests")
	}
	return s
}

// openBE opens the backend and sets the search path to public so integration
// tests resolve unqualified names correctly.
func openBE(t *testing.T) *postgres.Backend {
	t.Helper()
	be, err := postgres.Open(dsn(t))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	be.SetSchemas([]string{"public"})
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func TestIntegrationOpen(t *testing.T) {
	be := openBE(t)
	v := be.ServerVersion()
	if v.Major == 0 {
		t.Error("ServerVersion.Major = 0, want a real version")
	}
	t.Logf("connected to PostgreSQL %d.%d", v.Major, v.Minor)
}

func TestIntegrationIntrospect(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	// Seed a minimal table and clean up afterward.
	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_integ (
			id    serial PRIMARY KEY,
			label text NOT NULL,
			notes text
		)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_integ")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if model.Len() == 0 {
		t.Error("Introspect returned an empty model")
	}

	rel, ok := model.Lookup("_dbrest_test_integ", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_integ not found after introspect")
	}
	if len(rel.PrimaryKey) != 1 || rel.PrimaryKey[0] != "id" {
		t.Errorf("PrimaryKey = %v, want [id]", rel.PrimaryKey)
	}
	colNames := map[string]bool{}
	for _, c := range rel.Columns {
		colNames[c.Name] = true
	}
	for _, want := range []string{"id", "label", "notes"} {
		if !colNames[want] {
			t.Errorf("column %q not found in introspected relation", want)
		}
	}
}

func TestIntegrationReadWrite(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_rw (
			id    serial PRIMARY KEY,
			val   text NOT NULL
		)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_rw")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_rw", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_rw not found")
	}

	rc := &reqctx.Context{Role: "", Method: "POST", Path: "/_dbrest_test_rw"}
	writePlan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Insert,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_rw"},
			Write: &ir.WriteSpec{
				Rows:    []map[string]ir.Value{{"val": {JSON: "hello"}}},
				Columns: []string{"val"},
				Return:  ir.ReturnMinimal,
			},
		},
	}

	wres, err := be.Execute(ctx, writePlan, rc)
	if err != nil {
		t.Fatalf("Execute(insert): %v", err)
	}
	if aff, ok := wres.Affected(); !ok || aff != 1 {
		t.Errorf("Affected = (%d, %v), want (1, true)", aff, ok)
	}

	// Read it back.
	rc2 := &reqctx.Context{Role: "", Method: "GET", Path: "/_dbrest_test_rw"}
	readPlan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Read,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_rw"},
			Select:   []ir.SelectItem{ir.Column{Path: []string{"val"}}},
		},
	}
	rres, err := be.Execute(ctx, readPlan, rc2)
	if err != nil {
		t.Fatalf("Execute(read): %v", err)
	}
	rs := rres.Rows()
	defer rs.Close()
	count := 0
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values: %v", err)
		}
		if len(vals) == 0 {
			t.Error("expected at least one column")
		}
		count++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row error: %v", err)
	}
	if count == 0 {
		t.Error("read returned no rows after insert")
	}
}

func BenchmarkIntegrationRead(b *testing.B) {
	dsn := os.Getenv("DBREST_PG_DSN")
	if dsn == "" {
		b.Skip("DBREST_PG_DSN not set")
	}
	be, err := postgres.Open(dsn)
	if err != nil {
		b.Fatalf("postgres.Open: %v", err)
	}
	be.SetSchemas([]string{"public"})
	defer be.Close()

	ctx := context.Background()
	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_bench_read (
			id serial PRIMARY KEY, val text)`); err != nil {
		b.Fatalf("seed: %v", err)
	}
	defer be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_bench_read")

	model, _ := be.Introspect(ctx)
	rel, _ := model.Lookup("_dbrest_bench_read", []string{"public"})
	rc := &reqctx.Context{Method: "GET", Path: "/_dbrest_bench_read"}
	plan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Read,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_bench_read"},
			Select:   []ir.SelectItem{ir.Column{Path: []string{"id"}}, ir.Column{Path: []string{"val"}}},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		res, err := be.Execute(ctx, plan, rc)
		if err != nil {
			b.Fatal(err)
		}
		rs := res.Rows()
		for rs.Next() {
		}
		_ = rs.Err()
		rs.Close()
	}
}
