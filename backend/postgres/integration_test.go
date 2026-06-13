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

// TestIntegrationNativeCallPostFilter proves the native RPC path (plan.Func nil)
// applies select, filter, order, limit, and an exact count to a set-returning
// function's rows, the same post-filter a table read enjoys. Before the fix the
// native path ran SELECT * FROM fn(...) and silently dropped all of these.
// Finding 05-M08 / P01.
func TestIntegrationNativeCallPostFilter(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION _dbrest_test_films()
		RETURNS TABLE(id int, title text, year int)
		LANGUAGE sql STABLE AS $$
			SELECT * FROM (VALUES
				(1, 'Metropolis', 1927),
				(2, 'Blade Runner', 1982),
				(3, 'Arrival', 2016)
			) AS t(id, title, year)
		$$`); err != nil {
		t.Fatalf("seed function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP FUNCTION IF EXISTS _dbrest_test_films()")
	})

	// year >= 1982, ordered year desc, limit 1, projecting title only. Of the two
	// matching rows (Blade Runner 1982, Arrival 2016) the top of a year-desc order
	// is Arrival, and limit 1 keeps just that one.
	call := &ir.Call{
		Function: ir.Ref{Schema: "public", Name: "_dbrest_test_films"},
		Args:     map[string]ir.Value{},
		ReadOnly: true,
		Select:   []ir.SelectItem{ir.Column{Path: []string{"title"}}},
		Where:    condPtr(ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "1982"}}),
		Order:    []ir.OrderTerm{{Path: []string{"year"}, Desc: true}},
		Limit:    intPtr(1),
		Count:    ir.CountExact,
	}
	plan := &ir.Plan{ReadOnly: true, Call: call}

	res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/rpc/_dbrest_test_films"})
	if err != nil {
		t.Fatalf("Execute(native call): %v", err)
	}

	// The count is exact over the filtered set: two rows match year >= 1982.
	if c, ok := res.Count(); !ok || c != 2 {
		t.Errorf("Count = (%d, %v), want (2, true) over the filtered rows", c, ok)
	}

	rs := res.Rows()
	defer rs.Close()
	var titles []string
	var cols int
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values: %v", err)
		}
		cols = len(vals)
		titles = append(titles, vals[0].(string))
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row error: %v", err)
	}
	if len(titles) != 1 {
		t.Fatalf("limit 1 returned %d rows, want 1: %v", len(titles), titles)
	}
	if cols != 1 {
		t.Errorf("select=title returned %d columns, want 1", cols)
	}
	if titles[0] != "Arrival" {
		t.Errorf("order=year.desc top row = %q, want Arrival", titles[0])
	}
}

// TestIntegrationNativeCallSchemaDispatch proves a native RPC resolves in the
// request's negotiated schema (Accept-Profile / Content-Profile, carried as
// reqctx.Context.Schema), not always the first configured schema. Two schemas
// expose a same-named function with distinct results; switching rc.Schema picks
// the matching one. Finding 03-P04.
func TestIntegrationNativeCallSchemaDispatch(t *testing.T) {
	be := openBE(t)
	be.SetSchemas([]string{"_dbrest_s1", "_dbrest_s2"})
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS _dbrest_s1;
		CREATE SCHEMA IF NOT EXISTS _dbrest_s2;
		CREATE OR REPLACE FUNCTION _dbrest_s1.whoami() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT 'schema-one' $$;
		CREATE OR REPLACE FUNCTION _dbrest_s2.whoami() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT 'schema-two' $$`); err != nil {
		t.Fatalf("seed schemas: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS _dbrest_s1 CASCADE; DROP SCHEMA IF EXISTS _dbrest_s2 CASCADE")
	})

	call := func(schema string) string {
		t.Helper()
		plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
			Function: ir.Ref{Name: "whoami"},
			Args:     map[string]ir.Value{},
			ReadOnly: true,
		}}
		rc := &reqctx.Context{Method: "GET", Path: "/rpc/whoami", Schema: schema}
		res, err := be.Execute(ctx, plan, rc)
		if err != nil {
			t.Fatalf("Execute(%s): %v", schema, err)
		}
		rs := res.Rows()
		defer rs.Close()
		if !rs.Next() {
			t.Fatalf("Execute(%s): no rows", schema)
		}
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values(%s): %v", schema, err)
		}
		return vals[0].(string)
	}

	if got := call("_dbrest_s1"); got != "schema-one" {
		t.Errorf("Accept-Profile _dbrest_s1 dispatched to %q, want schema-one", got)
	}
	if got := call("_dbrest_s2"); got != "schema-two" {
		t.Errorf("Accept-Profile _dbrest_s2 dispatched to %q, want schema-two", got)
	}
}

// TestIntegrationNativeCallJSONArg proves a JSON object argument binds to a
// json, a jsonb, and a text parameter alike. The argument is spliced as an
// untyped literal so PostgreSQL's function resolution coerces it to whichever
// type the parameter declares; a '...'::json literal would fail to match a
// jsonb parameter (42883 -> 404). Finding 03-P05.
func TestIntegrationNativeCallJSONArg(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION _dbrest_test_jb(payload jsonb) RETURNS text
			LANGUAGE sql IMMUTABLE AS $$ SELECT payload->>'name' $$;
		CREATE OR REPLACE FUNCTION _dbrest_test_js(payload json) RETURNS text
			LANGUAGE sql IMMUTABLE AS $$ SELECT payload->>'name' $$;
		CREATE OR REPLACE FUNCTION _dbrest_test_tx(payload text) RETURNS text
			LANGUAGE sql IMMUTABLE AS $$ SELECT payload $$`); err != nil {
		t.Fatalf("seed functions: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `
			DROP FUNCTION IF EXISTS _dbrest_test_jb(jsonb);
			DROP FUNCTION IF EXISTS _dbrest_test_js(json);
			DROP FUNCTION IF EXISTS _dbrest_test_tx(text)`)
	})

	call := func(fn string) string {
		t.Helper()
		plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
			Function: ir.Ref{Name: fn},
			Args:     map[string]ir.Value{"payload": {JSON: map[string]any{"name": "Ada"}}},
			ReadOnly: true,
		}}
		res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/rpc/" + fn})
		if err != nil {
			t.Fatalf("Execute(%s): %v", fn, err)
		}
		rs := res.Rows()
		defer rs.Close()
		if !rs.Next() {
			t.Fatalf("Execute(%s): no rows", fn)
		}
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values(%s): %v", fn, err)
		}
		if vals[0] == nil {
			return ""
		}
		return vals[0].(string)
	}

	// json/jsonb parameters extract the name; the text parameter receives the
	// serialized object. The point is that none of the three 404s.
	if got := call("_dbrest_test_jb"); got != "Ada" {
		t.Errorf("jsonb arg returned %q, want Ada", got)
	}
	if got := call("_dbrest_test_js"); got != "Ada" {
		t.Errorf("json arg returned %q, want Ada", got)
	}
	if got := call("_dbrest_test_tx"); got != `{"name":"Ada"}` {
		t.Errorf("text arg returned %q, want the serialized object", got)
	}
}

func condPtr(c ir.Cond) *ir.Cond { return &c }
func intPtr(n int) *int          { return &n }

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
