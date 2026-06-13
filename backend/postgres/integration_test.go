package postgres_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/postgres"
	"github.com/tamnd/dbrest/ir"
	planpkg "github.com/tamnd/dbrest/plan"
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

// TestIntegrationTemporalRendering proves date, time, timetz, interval,
// timestamp, and timestamptz columns render through the backend as the same JSON
// strings PostgreSQL itself emits (to_json), instead of Go struct or Z-suffixed
// RFC3339 spellings. The expected values are read back from the server with
// to_json so the assertion tracks the live server's TimeZone. Finding 03-P07.
func TestIntegrationTemporalRendering(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_temporal (
			id   int PRIMARY KEY,
			d    date,
			t    time,
			ttz  timetz,
			iv   interval,
			ts   timestamp,
			tstz timestamptz
		);
		TRUNCATE _dbrest_test_temporal;
		INSERT INTO _dbrest_test_temporal VALUES (
			1,
			'2017-01-02',
			'13:00:00.5',
			'13:00:00+02',
			'1 day 02:03:04.5',
			'2017-01-01 14:30:00.123456',
			'2017-07-01 14:30:00+05'
		)`); err != nil {
		t.Fatalf("seed temporal table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_temporal")
	})

	cols := []string{"d", "t", "ttz", "iv", "ts", "tstz"}
	// The oracle: PostgreSQL's own JSON spelling for each column, stripped of the
	// surrounding quotes a JSON string carries.
	want := make([]string, len(cols))
	for i, c := range cols {
		var j string
		if err := be.Pool().QueryRow(ctx,
			"SELECT to_json("+c+")::text FROM _dbrest_test_temporal WHERE id = 1").Scan(&j); err != nil {
			t.Fatalf("oracle to_json(%s): %v", c, err)
		}
		want[i] = strings.Trim(j, `"`)
	}

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_temporal", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_temporal not found")
	}
	sel := make([]ir.SelectItem, len(cols))
	for i, c := range cols {
		sel[i] = ir.Column{Path: []string{c}}
	}
	plan := &ir.Plan{Rel: rel, Query: &ir.Query{
		Kind:     ir.Read,
		Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_temporal"},
		Select:   sel,
	}}
	res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/_dbrest_test_temporal"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rs := res.Rows()
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("no rows")
	}
	vals, err := rs.Values()
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	for i, c := range cols {
		got, ok := vals[i].(string)
		if !ok {
			t.Errorf("column %s rendered as %T (%v), want a string", c, vals[i], vals[i])
			continue
		}
		if got != want[i] {
			t.Errorf("column %s = %q, want %q (PostgreSQL to_json)", c, got, want[i])
		}
	}
}

// TestIntegrationFullTextTSVector proves an fts filter on a real tsvector column
// returns rows instead of failing. PostgreSQL has no to_tsvector(tsvector)
// overload, so wrapping the column raised 42883 (surfaced as 404). With the
// column type threaded through, the dialect matches the column directly
// (col @@ to_tsquery(...)), the way PostgREST does. Finding 01-P01.
func TestIntegrationFullTextTSVector(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_fts (
			id  serial PRIMARY KEY,
			doc tsvector NOT NULL
		);
		TRUNCATE _dbrest_test_fts;
		INSERT INTO _dbrest_test_fts (doc) VALUES
			(to_tsvector('english', 'the quick brown fox')),
			(to_tsvector('english', 'a lazy dog sleeps'))`); err != nil {
		t.Fatalf("seed tsvector table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_fts")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_fts", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_fts not found")
	}

	// fts on the tsvector column: ?doc=fts.fox should match only the first row.
	// ColumnType is "tsvector" as the planner resolves it from the schema.
	plan := &ir.Plan{Rel: rel, Query: &ir.Query{
		Kind:     ir.Read,
		Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_fts"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"id"}}},
		Where: condPtr(ir.Compare{
			Path:       []string{"doc"},
			Op:         ir.OpFTS,
			FTS:        ir.FTSPlain,
			Value:      ir.Value{Text: "fox"},
			ColumnType: "tsvector",
		}),
	}}
	res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/_dbrest_test_fts"})
	if err != nil {
		t.Fatalf("Execute(fts on tsvector): %v", err)
	}
	rs := res.Rows()
	defer rs.Close()
	rows := 0
	for rs.Next() {
		if _, err := rs.Values(); err != nil {
			t.Fatalf("Values: %v", err)
		}
		rows++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row error: %v", err)
	}
	if rows != 1 {
		t.Errorf("fts.fox matched %d rows, want 1", rows)
	}
}

// TestIntegrationArrayPayloadByColumnType proves a JSON array payload value
// lands as JSON in a jsonb column and as a PostgreSQL array in a text[] column.
// Before the fix every array became a {a,b} literal, so inserting an array into
// a jsonb column failed with 22P02. The planner resolves the target column type
// and the dialect routes the value accordingly. Finding 01-P06.
func TestIntegrationArrayPayloadByColumnType(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_arr (
			id   serial PRIMARY KEY,
			tags jsonb NOT NULL,
			labs text[] NOT NULL
		);
		TRUNCATE _dbrest_test_arr`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_arr")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_arr", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_arr not found")
	}

	// The planner fills WriteSpec.ColumnTypes from the relation; build the same
	// shape here so the compiler routes each array by its target column type.
	plan := &ir.Plan{Rel: rel, Query: &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_arr"},
		Write: &ir.WriteSpec{
			Rows: []map[string]ir.Value{{
				"tags": {JSON: []any{"x", "y"}},
				"labs": {JSON: []any{"a", "b"}},
			}},
			Columns:     []string{"tags", "labs"},
			ColumnTypes: map[string]string{"tags": "jsonb", "labs": "text[]"},
			Return:      ir.ReturnMinimal,
		},
	}}
	if _, err := be.Execute(ctx, plan, &reqctx.Context{Method: "POST", Path: "/_dbrest_test_arr"}); err != nil {
		t.Fatalf("Execute(insert arrays): %v", err)
	}

	// Read the stored values straight from the pool to confirm the jsonb holds a
	// JSON array and the text[] holds two elements.
	var tags string
	var labs []string
	if err := be.Pool().QueryRow(ctx,
		"SELECT tags::text, labs FROM _dbrest_test_arr LIMIT 1").Scan(&tags, &labs); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if tags != `["x", "y"]` {
		t.Errorf("jsonb tags = %q, want a JSON array", tags)
	}
	if len(labs) != 2 || labs[0] != "a" || labs[1] != "b" {
		t.Errorf("text[] labs = %v, want [a b]", labs)
	}
}

// TestIntegrationWideEmbed proves an embed of a table with more than 50 columns
// assembles instead of failing. json_build_object caps at 100 arguments (two per
// key), so a 60-column embed raised 54023; the dialect now chunks the object with
// jsonb_build_object and || past 50 keys. Finding 01-P07.
func TestIntegrationWideEmbed(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	// A parent with a child whose 60 columns force the chunked path.
	var childCols strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&childCols, ", c%d int DEFAULT %d", i, i)
	}
	ddl := `
		CREATE TABLE IF NOT EXISTS _dbrest_test_parent (id int PRIMARY KEY);
		CREATE TABLE IF NOT EXISTS _dbrest_test_child (
			id int PRIMARY KEY,
			parent_id int REFERENCES _dbrest_test_parent(id)` + childCols.String() + `
		);
		TRUNCATE _dbrest_test_child, _dbrest_test_parent;
		INSERT INTO _dbrest_test_parent (id) VALUES (1);
		INSERT INTO _dbrest_test_child (id, parent_id) VALUES (10, 1);`
	if _, err := be.Pool().Exec(ctx, ddl); err != nil {
		t.Fatalf("seed wide tables: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_child; DROP TABLE IF EXISTS _dbrest_test_parent")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_parent", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_parent not found")
	}

	// GET /_dbrest_test_parent?select=id,_dbrest_test_child(*) embeds every child
	// column, which is the chunked-object case.
	q, perr := ir.ParseRead("_dbrest_test_parent", "select=id,_dbrest_test_child(*)", nil)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	rp, perr := planpkg.Read(model, q, []string{"public"}, planpkg.Options{})
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	rp.Rel = rel

	res, err := be.Execute(ctx, rp, &reqctx.Context{Method: "GET", Path: "/_dbrest_test_parent"})
	if err != nil {
		t.Fatalf("Execute(wide embed): %v", err)
	}
	rs := res.Rows()
	defer rs.Close()
	rows := 0
	for rs.Next() {
		if _, err := rs.Values(); err != nil {
			t.Fatalf("Values: %v", err)
		}
		rows++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row error: %v", err)
	}
	if rows != 1 {
		t.Errorf("wide embed returned %d parent rows, want 1", rows)
	}
}

// TestIntegrationCountedReadConsistent exercises the counted-read path, which
// runs the count and the page as two statements. The fix pins that transaction
// to REPEATABLE READ so both statements read one snapshot, the way PostgREST's
// single statement does. The test seeds a known set, reads it with a page
// smaller than the total, and proves the exact count reports the whole set while
// the page honours the limit. Finding P11.
func TestIntegrationCountedReadConsistent(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_counted (id serial PRIMARY KEY);
		TRUNCATE _dbrest_test_counted;
		INSERT INTO _dbrest_test_counted SELECT generate_series(1, 7)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_counted")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_counted", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_counted not found")
	}

	plan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Read,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_counted"},
			Select:   []ir.SelectItem{ir.Column{Path: []string{"id"}}},
			Limit:    intPtr(3),
			Count:    ir.CountExact,
		},
	}

	res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/_dbrest_test_counted"})
	if err != nil {
		t.Fatalf("Execute(counted read): %v", err)
	}
	if c, ok := res.Count(); !ok || c != 7 {
		t.Errorf("Count = (%d, %v), want (7, true) over the whole set", c, ok)
	}
	rs := res.Rows()
	defer rs.Close()
	page := 0
	for rs.Next() {
		if _, err := rs.Values(); err != nil {
			t.Fatalf("Values: %v", err)
		}
		page++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row error: %v", err)
	}
	if page != 3 {
		t.Errorf("page returned %d rows, want 3 (the limit)", page)
	}
}

// TestIntegrationUpsertNoConflictTarget proves a merge upsert against a table
// with no primary key degrades to a plain INSERT instead of emitting an invalid
// ON CONFLICT DO UPDATE. This matches PostgREST 14, where a merge-duplicates POST
// to a key-less table inserts the rows and returns 201 (verified against a live
// PostgREST). Two identical rows therefore both land. Finding P12.
func TestIntegrationUpsertNoConflictTarget(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_nopk (a int, b text);
		TRUNCATE _dbrest_test_nopk`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_nopk")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_nopk", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_nopk not found")
	}

	plan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Upsert,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_nopk"},
			Write: &ir.WriteSpec{
				Rows:     []map[string]ir.Value{{"a": {JSON: "1"}, "b": {JSON: "x"}}},
				Columns:  []string{"a", "b"},
				Return:   ir.ReturnMinimal,
				Conflict: &ir.Conflict{Resolution: ir.ConflictMerge},
			},
		},
	}
	rc := &reqctx.Context{Method: "POST", Path: "/_dbrest_test_nopk"}

	for i := 0; i < 2; i++ {
		if _, err := be.Execute(ctx, plan, rc); err != nil {
			t.Fatalf("Execute(merge upsert, no PK) #%d: %v", i, err)
		}
	}
	var n int
	if err := be.Pool().QueryRow(ctx, "SELECT count(*) FROM _dbrest_test_nopk WHERE a=1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("rows after two merge upserts = %d, want 2 (plain insert, no merge)", n)
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
