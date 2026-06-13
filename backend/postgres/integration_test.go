package postgres_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/postgres"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	planpkg "github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
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

// TestIntegrationInListAny proves the col = ANY($1) lowering selects exactly the
// rows an expanded IN would, against a live server. The list binds as one array
// literal parameter. Finding P13.
func TestIntegrationInListAny(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_inlist (id int PRIMARY KEY);
		TRUNCATE _dbrest_test_inlist;
		INSERT INTO _dbrest_test_inlist SELECT generate_series(1, 5)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, "DROP TABLE IF EXISTS _dbrest_test_inlist")
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_inlist", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_inlist not found")
	}

	plan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Read,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_inlist"},
			Select:   []ir.SelectItem{ir.Column{Path: []string{"id"}}},
			Where:    condPtr(ir.Compare{Path: []string{"id"}, Op: ir.OpIn, ColumnType: "integer", Value: ir.Value{List: []string{"2", "4", "9"}}}),
			Order:    []ir.OrderTerm{{Path: []string{"id"}}},
		},
	}
	res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/_dbrest_test_inlist"})
	if err != nil {
		t.Fatalf("Execute(in-list): %v", err)
	}
	rs := res.Rows()
	defer rs.Close()
	var got []int32
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values: %v", err)
		}
		got = append(got, vals[0].(int32))
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row error: %v", err)
	}
	// 2 and 4 exist; 9 does not. = ANY selects exactly the present members.
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("in-list rows = %v, want [2 4]", got)
	}
}

// TestIntegrationSearchPathShape proves the per-request search_path is the active
// schema followed by db-extra-search-path (default "public"), not the whole
// exposed schema set, and that the GUC string is the verbatim quoted value
// PostgREST writes. It reads current_setting('search_path') through a native RPC
// and switches the active schema via Accept-Profile (reqctx.Context.Schema).
// Finding 02-P01. Verified against PostgREST 14.12, which sets the path with
// set_config('search_path', '"<active>", "public"', true) and does not dedup, so
// an active schema of public yields "public", "public".
func TestIntegrationSearchPathShape(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS _dbrest_sp1;
		CREATE SCHEMA IF NOT EXISTS _dbrest_sp2;
		CREATE OR REPLACE FUNCTION public.show_path() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT current_setting('search_path') $$;
		CREATE OR REPLACE FUNCTION _dbrest_sp1.show_path() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT current_setting('search_path') $$;
		CREATE OR REPLACE FUNCTION _dbrest_sp2.show_path() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT current_setting('search_path') $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public.show_path();
			DROP SCHEMA IF EXISTS _dbrest_sp1 CASCADE; DROP SCHEMA IF EXISTS _dbrest_sp2 CASCADE`)
	})

	path := func(schema string) string {
		t.Helper()
		plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
			Function: ir.Ref{Name: "show_path"},
			Args:     map[string]ir.Value{},
			ReadOnly: true,
		}}
		res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/rpc/show_path", Schema: schema})
		if err != nil {
			t.Fatalf("Execute(%q): %v", schema, err)
		}
		rs := res.Rows()
		defer rs.Close()
		if !rs.Next() {
			t.Fatalf("Execute(%q): no rows", schema)
		}
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values(%q): %v", schema, err)
		}
		return vals[0].(string)
	}

	// Default active schema is public (the single configured schema), extra is the
	// default "public"; PostgREST does not dedup, so the path is "public", "public".
	be.SetSchemas([]string{"public"})
	be.SetExtraSearchPath([]string{"public"})
	if got := path(""); got != `"public", "public"` {
		t.Errorf(`default search_path = %q, want "public", "public"`, got)
	}

	// Two exposed schemas: the active one (Accept-Profile) leads the path, not the
	// first configured schema, and the whole set never appears.
	be.SetSchemas([]string{"_dbrest_sp1", "_dbrest_sp2"})
	if got := path("_dbrest_sp1"); got != `"_dbrest_sp1", "public"` {
		t.Errorf(`sp1 search_path = %q, want "_dbrest_sp1", "public"`, got)
	}
	if got := path("_dbrest_sp2"); got != `"_dbrest_sp2", "public"` {
		t.Errorf(`sp2 search_path = %q, want "_dbrest_sp2", "public"`, got)
	}
}

// TestIntegrationSearchPathReachesExtra proves db-extra-search-path puts its
// schemas on the path: a function running in a non-public active schema resolves
// an unqualified helper defined in public because public is appended to the path.
// Finding 02-P01.
func TestIntegrationSearchPathReachesExtra(t *testing.T) {
	be := openBE(t)
	be.SetSchemas([]string{"_dbrest_spx"})
	be.SetExtraSearchPath([]string{"public"})
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS _dbrest_spx;
		CREATE OR REPLACE FUNCTION public._dbrest_helper() RETURNS text
			LANGUAGE sql IMMUTABLE AS $$ SELECT 'from-public' $$;
		CREATE OR REPLACE FUNCTION _dbrest_spx.uses_helper() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT _dbrest_helper() $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_helper();
			DROP SCHEMA IF EXISTS _dbrest_spx CASCADE`)
	})

	plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
		Function: ir.Ref{Name: "uses_helper"},
		Args:     map[string]ir.Value{},
		ReadOnly: true,
	}}
	res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/rpc/uses_helper", Schema: "_dbrest_spx"})
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
	if got := vals[0].(string); got != "from-public" {
		t.Errorf("unqualified helper resolved to %q, want from-public", got)
	}
}

// TestIntegrationNativeCallVolatilityAccessMode proves the native RPC access mode
// follows the function's volatility, not only the HTTP method: a POST to a STABLE
// or IMMUTABLE function runs in a read-only transaction, while a VOLATILE function
// runs read-write, matching PostgREST's access-mode table. Each function reports
// current_setting('transaction_read_only') so the transaction mode is observed
// directly, and a volatile insert proves the read-write path still commits.
// Finding 02-P06. Verified against PostgREST 14.12.
func TestIntegrationNativeCallVolatilityAccessMode(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_vol (n int);
		TRUNCATE _dbrest_test_vol;
		CREATE OR REPLACE FUNCTION public._dbrest_txmode_v() RETURNS text
			LANGUAGE sql VOLATILE AS $$ SELECT current_setting('transaction_read_only') $$;
		CREATE OR REPLACE FUNCTION public._dbrest_txmode_s() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT current_setting('transaction_read_only') $$;
		CREATE OR REPLACE FUNCTION public._dbrest_vol_insert(x int) RETURNS int
			LANGUAGE sql VOLATILE AS $$ INSERT INTO _dbrest_test_vol VALUES (x) RETURNING n $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_txmode_v();
			DROP FUNCTION IF EXISTS public._dbrest_txmode_s();
			DROP FUNCTION IF EXISTS public._dbrest_vol_insert(int);
			DROP TABLE IF EXISTS _dbrest_test_vol`)
	})

	// Refresh the catalog so the new functions' volatility is loaded.
	if _, err := be.Introspect(ctx); err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	txmode := func(fn, method string) string {
		t.Helper()
		plan := &ir.Plan{
			ReadOnly: method == "GET",
			Call:     &ir.Call{Function: ir.Ref{Name: fn}, Args: map[string]ir.Value{}},
		}
		res, err := be.Execute(ctx, plan, &reqctx.Context{Method: method, Path: "/rpc/" + fn})
		if err != nil {
			t.Fatalf("Execute(%s %s): %v", method, fn, err)
		}
		rs := res.Rows()
		defer rs.Close()
		if !rs.Next() {
			t.Fatalf("Execute(%s %s): no rows", method, fn)
		}
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values(%s %s): %v", method, fn, err)
		}
		return vals[0].(string)
	}

	// POST to a VOLATILE function runs read-write; POST to a STABLE function runs
	// read-only (the fix); GET to either is read-only.
	if got := txmode("_dbrest_txmode_v", "POST"); got != "off" {
		t.Errorf("volatile POST transaction_read_only = %q, want off", got)
	}
	if got := txmode("_dbrest_txmode_s", "POST"); got != "on" {
		t.Errorf("stable POST transaction_read_only = %q, want on (read-only)", got)
	}
	if got := txmode("_dbrest_txmode_s", "GET"); got != "on" {
		t.Errorf("stable GET transaction_read_only = %q, want on", got)
	}

	// The read-write path still commits: a volatile insert via POST persists.
	volPlan := &ir.Plan{Call: &ir.Call{
		Function: ir.Ref{Name: "_dbrest_vol_insert"},
		Args:     map[string]ir.Value{"x": {Text: "7"}},
	}}
	if _, err := be.Execute(ctx, volPlan, &reqctx.Context{Method: "POST", Path: "/rpc/_dbrest_vol_insert"}); err != nil {
		t.Fatalf("volatile insert POST: %v", err)
	}
	var n int
	if err := be.Pool().QueryRow(ctx, "SELECT count(*) FROM _dbrest_test_vol").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("after volatile insert POST rows = %d, want 1", n)
	}
}

// TestIntegrationImpersonatedRoleSettings proves the backend replays an
// impersonated role's ALTER ROLE ... SET settings as transaction-scoped settings,
// like PostgREST: a role pinned to statement_timeout '50ms' carries that timeout
// on every request, a slow call as that role is cancelled (SQLSTATE 57014 -> 500),
// and the setting is transaction-scoped so it does not leak to a request that runs
// without the role. Finding 02-P02. Verified against PostgREST 14.12.
func TestIntegrationImpersonatedRoleSettings(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	// A role granted to the connected authenticator, pinned to a short timeout, so
	// loadRoleSettings (which reads roles the authenticator is a member of) picks it
	// up. Functions are PUBLIC-executable by default, so the role can call them.
	if _, err := be.Pool().Exec(ctx, `
		DROP ROLE IF EXISTS _dbrest_slow;
		CREATE ROLE _dbrest_slow;
		GRANT _dbrest_slow TO CURRENT_USER;
		ALTER ROLE _dbrest_slow SET statement_timeout = '50ms';
		CREATE OR REPLACE FUNCTION public._dbrest_show_timeout() RETURNS text
			LANGUAGE sql STABLE AS $$ SELECT current_setting('statement_timeout') $$;
		CREATE OR REPLACE FUNCTION public._dbrest_sleep() RETURNS text
			LANGUAGE sql VOLATILE AS $$ SELECT pg_sleep(3)::text $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_show_timeout();
			DROP FUNCTION IF EXISTS public._dbrest_sleep();
			DROP ROLE IF EXISTS _dbrest_slow`)
	})

	// Refresh the catalog so the role's settings are loaded.
	if _, err := be.Introspect(ctx); err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	showTimeout := func(role string) string {
		t.Helper()
		plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
			Function: ir.Ref{Name: "_dbrest_show_timeout"},
			Args:     map[string]ir.Value{},
		}}
		res, err := be.Execute(ctx, plan, &reqctx.Context{Role: role, Method: "GET", Path: "/rpc/_dbrest_show_timeout"})
		if err != nil {
			t.Fatalf("show_timeout(%q): %v", role, err)
		}
		rs := res.Rows()
		defer rs.Close()
		if !rs.Next() {
			t.Fatalf("show_timeout(%q): no rows", role)
		}
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values(%q): %v", role, err)
		}
		return vals[0].(string)
	}

	// The role carries its pinned timeout.
	if got := showTimeout("_dbrest_slow"); got != "50ms" {
		t.Errorf("statement_timeout as _dbrest_slow = %q, want 50ms", got)
	}
	// A request without the role does not inherit it (transaction-scoped, no leak).
	if got := showTimeout(""); got == "50ms" {
		t.Errorf("statement_timeout without the role = %q, want the server default (not 50ms)", got)
	}

	// A slow call as the role is cancelled by the pinned timeout.
	sleepPlan := &ir.Plan{Call: &ir.Call{
		Function: ir.Ref{Name: "_dbrest_sleep"},
		Args:     map[string]ir.Value{},
	}}
	_, err := be.Execute(ctx, sleepPlan, &reqctx.Context{Role: "_dbrest_slow", Method: "POST", Path: "/rpc/_dbrest_sleep"})
	if err == nil {
		t.Fatal("slow call as _dbrest_slow: want a timeout error, got nil")
	}
	apiErr, ok := err.(*pgerr.APIError)
	if !ok {
		t.Fatalf("timeout error type = %T, want *pgerr.APIError", err)
	}
	if apiErr.Code != "57014" {
		t.Errorf("timeout code = %q, want 57014", apiErr.Code)
	}
	if apiErr.HTTPStatus != 500 {
		t.Errorf("timeout status = %d, want 500", apiErr.HTTPStatus)
	}
}

// TestIntegrationReadCallResponseControls proves a STABLE function reached over GET
// can still steer its response: response.status and response.headers it sets are
// read back and folded into the response controls. Before the fix the read-call
// path streamed straight from the cursor and never called readResponseControls, so
// the GUCs a function set on a GET were silently dropped. Finding 02-P05.
func TestIntegrationReadCallResponseControls(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	// A STABLE function (so the call runs read-only, the path under test) that sets a
	// status override and a Cache-Control response header the PostgREST way: a JSON
	// array of single-key name->value objects in response.headers.
	if _, err := be.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION public._dbrest_resp_ctl() RETURNS text
			LANGUAGE plpgsql STABLE AS $$
		BEGIN
			PERFORM set_config('response.status', '205', true);
			PERFORM set_config('response.headers', '[{"Cache-Control": "max-age=60"}]', true);
			RETURN 'ok';
		END $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_resp_ctl()`)
	})

	if _, err := be.Introspect(ctx); err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
		Function: ir.Ref{Name: "_dbrest_resp_ctl"},
		Args:     map[string]ir.Value{},
	}}
	rc := &reqctx.Context{Method: "GET", Path: "/rpc/_dbrest_resp_ctl"}
	res, err := be.Execute(ctx, plan, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ctrl := res.ResponseControls()
	if ctrl.Status != 205 {
		t.Errorf("response status override = %d, want 205", ctrl.Status)
	}
	if got := ctrl.Headers["Cache-Control"]; got != "max-age=60" {
		t.Errorf("Cache-Control header = %q, want max-age=60", got)
	}

	// The body still carries the function's return value.
	rs := res.Rows()
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("Execute: no rows")
	}
	vals, err := rs.Values()
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if vals[0].(string) != "ok" {
		t.Errorf("body = %q, want ok", vals[0].(string))
	}
}

// TestIntegrationReadTablePreRequestControls proves a db-pre-request function can
// steer the response of a plain GET table read: a header it sets via
// response.headers is read back before the body streams. Before the fix the
// table-read path streamed from the cursor and never read the response GUCs, so a
// pre-request that set a header on a GET was silently dropped. Finding 02-P05.
func TestIntegrationReadTablePreRequestControls(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_pr (id serial PRIMARY KEY, val text);
		TRUNCATE _dbrest_test_pr;
		INSERT INTO _dbrest_test_pr (val) VALUES ('a');
		CREATE OR REPLACE FUNCTION public._dbrest_pre() RETURNS void
			LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM set_config('response.headers', '[{"X-Pre": "ran"}]', true);
		END $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_pre();
			DROP TABLE IF EXISTS _dbrest_test_pr`)
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_pr", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_pr not found")
	}

	rc := &reqctx.Context{Method: "GET", Path: "/_dbrest_test_pr", PreRequest: "_dbrest_pre"}
	readPlan := &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Read,
			Relation: ir.Ref{Schema: "public", Name: "_dbrest_test_pr"},
			Select:   []ir.SelectItem{ir.Column{Path: []string{"val"}}},
		},
	}
	res, err := be.Execute(ctx, readPlan, rc)
	if err != nil {
		t.Fatalf("Execute(read): %v", err)
	}
	if got := res.ResponseControls().Headers["X-Pre"]; got != "ran" {
		t.Errorf("X-Pre header = %q, want ran (pre-request header dropped on table read)", got)
	}
	// The body still streams the row.
	rs := res.Rows()
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("read returned no rows")
	}
}

// TestIntegrationHoistedTxSettings proves db-hoisted-tx-settings: a function's SET
// clause for a hoisted setting is applied to the transaction, not only the
// function body. default_transaction_isolation is the cleanest probe because it
// can never take effect without hoisting (the transaction has already started by
// the time the function runs), so a function that returns the current isolation
// level reads the database default unless its SET clause was hoisted to BeginTx.
// Finding 02-P03.
func TestIntegrationHoistedTxSettings(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION public._dbrest_hoist_iso() RETURNS text
			LANGUAGE sql STABLE SET default_transaction_isolation = 'serializable'
			AS $$ SELECT current_setting('transaction_isolation') $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_hoist_iso()`)
	})

	if _, err := be.Introspect(ctx); err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	callIso := func() string {
		t.Helper()
		plan := &ir.Plan{ReadOnly: true, Call: &ir.Call{
			Function: ir.Ref{Name: "_dbrest_hoist_iso"},
			Args:     map[string]ir.Value{},
		}}
		res, err := be.Execute(ctx, plan, &reqctx.Context{Method: "GET", Path: "/rpc/_dbrest_hoist_iso"})
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
		return vals[0].(string)
	}

	// With no hoisted settings configured, the function's SET clause stays inside
	// the body and the transaction runs at the database default.
	if got := callIso(); got == "serializable" {
		t.Errorf("isolation without hoisting = %q, want the default (not serializable)", got)
	}

	// With the v14 default hoist list, default_transaction_isolation is applied at
	// BeginTx, so the transaction itself runs serializable.
	be.SetHoistedTxSettings([]string{"statement_timeout", "plan_filter.statement_cost_limit", "default_transaction_isolation"})
	if got := callIso(); got != "serializable" {
		t.Errorf("isolation with hoisting = %q, want serializable", got)
	}
}

// TestIntegrationRelationKinds proves the schema cache mirrors PostgREST's
// relation set: a materialized view is exposed (as the view kind), a foreign table
// is exposed (as the table kind), and a partitioned table exposes only the parent,
// never its leaf partitions. Before the fix the relkind filter was IN ('r','v','p')
// with no relispartition guard, so matviews and foreign tables were invisible and
// every partition leaked in as its own endpoint. Finding 03-P08 / 03-P14.
func TestIntegrationRelationKinds(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	// A matview over a base table, a partitioned parent with two leaf partitions,
	// and a foreign table over a file_fdw server. file_fdw ships with the standard
	// contrib package and needs no network, so it is the lightest foreign table to
	// stand up; if the extension is unavailable the foreign-table leg is skipped.
	if _, err := be.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _dbrest_test_mvbase (id int PRIMARY KEY, n int);
		TRUNCATE _dbrest_test_mvbase;
		INSERT INTO _dbrest_test_mvbase VALUES (1, 10), (2, 20);
		DROP MATERIALIZED VIEW IF EXISTS _dbrest_test_mv;
		CREATE MATERIALIZED VIEW _dbrest_test_mv AS SELECT id, n FROM _dbrest_test_mvbase;
		CREATE TABLE IF NOT EXISTS _dbrest_test_part (id int, region text) PARTITION BY LIST (region);
		CREATE TABLE IF NOT EXISTS _dbrest_test_part_us PARTITION OF _dbrest_test_part FOR VALUES IN ('us');
		CREATE TABLE IF NOT EXISTS _dbrest_test_part_eu PARTITION OF _dbrest_test_part FOR VALUES IN ('eu')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `
			DROP MATERIALIZED VIEW IF EXISTS _dbrest_test_mv;
			DROP TABLE IF EXISTS _dbrest_test_mvbase;
			DROP TABLE IF EXISTS _dbrest_test_part`)
	})

	// Best-effort foreign table over file_fdw; the test still asserts the matview
	// and partition behaviour when the extension is not installed.
	haveForeign := false
	if _, err := be.Pool().Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS file_fdw;
		DROP SERVER IF EXISTS _dbrest_test_files CASCADE;
		CREATE SERVER _dbrest_test_files FOREIGN DATA WRAPPER file_fdw;
		CREATE FOREIGN TABLE _dbrest_test_ft (line text)
			SERVER _dbrest_test_files OPTIONS (filename '/etc/hostname')`); err == nil {
		haveForeign = true
		t.Cleanup(func() {
			_, _ = be.Pool().Exec(ctx, `DROP SERVER IF EXISTS _dbrest_test_files CASCADE`)
		})
	} else {
		t.Logf("file_fdw unavailable, skipping foreign-table leg: %v", err)
	}

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	// The materialized view is exposed and carries the view kind.
	mv, ok := model.Lookup("_dbrest_test_mv", []string{"public"})
	if !ok {
		t.Fatal("materialized view _dbrest_test_mv not exposed")
	}
	if mv.Kind != schema.KindView {
		t.Errorf("matview kind = %v, want KindView", mv.Kind)
	}

	// The partitioned parent is exposed; the leaf partitions are not.
	if _, ok := model.Lookup("_dbrest_test_part", []string{"public"}); !ok {
		t.Error("partitioned parent _dbrest_test_part not exposed")
	}
	if _, ok := model.Lookup("_dbrest_test_part_us", []string{"public"}); ok {
		t.Error("leaf partition _dbrest_test_part_us leaked as an endpoint")
	}
	if _, ok := model.Lookup("_dbrest_test_part_eu", []string{"public"}); ok {
		t.Error("leaf partition _dbrest_test_part_eu leaked as an endpoint")
	}

	// The foreign table is exposed and carries the table kind (an FDW can write).
	if haveForeign {
		ft, ok := model.Lookup("_dbrest_test_ft", []string{"public"})
		if !ok {
			t.Error("foreign table _dbrest_test_ft not exposed")
		} else if ft.Kind != schema.KindTable {
			t.Errorf("foreign table kind = %v, want KindTable", ft.Kind)
		}
	}
}

// TestIntegrationCatalogMetadata proves the introspector populates the catalog
// metadata PostgREST's schema cache carries and dbrest's frontend already
// consumes: unique constraints and unique indexes (one-to-one detection, P10),
// identity columns folded into HasDefault with the Identity flag set (P15), and
// table, column, and schema comments (P16). Before the fix none of these reached
// the model: unique sets were empty, identity columns looked default-less, and the
// model carried no descriptions.
func TestIntegrationCatalogMetadata(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		DROP TABLE IF EXISTS _dbrest_test_meta;
		CREATE TABLE _dbrest_test_meta (
			id    int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email text NOT NULL UNIQUE,
			slug  text NOT NULL,
			tenant int NOT NULL,
			label text
		);
		CREATE UNIQUE INDEX _dbrest_test_meta_slug_tenant ON _dbrest_test_meta (slug, tenant);
		COMMENT ON TABLE _dbrest_test_meta IS 'People records';
		COMMENT ON COLUMN _dbrest_test_meta.email IS 'Primary contact email';
		COMMENT ON SCHEMA public IS 'The default schema'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP TABLE IF EXISTS _dbrest_test_meta;
			COMMENT ON SCHEMA public IS NULL`)
	})

	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("_dbrest_test_meta", []string{"public"})
	if !ok {
		t.Fatal("_dbrest_test_meta not found")
	}

	// P15: the identity column is folded into HasDefault and flags Identity.
	idCol, ok := rel.Column("id")
	if !ok {
		t.Fatal("id column missing")
	}
	if !idCol.Identity {
		t.Error("id Identity = false, want true (GENERATED ALWAYS AS IDENTITY)")
	}
	if !idCol.HasDefault {
		t.Error("id HasDefault = false, want true (identity column is server-generated)")
	}

	// P10: the single-column unique constraint on email and the composite unique
	// index on (slug, tenant) both reach the model; the PK is not duplicated here.
	hasUnique := func(want ...string) bool {
		for _, u := range rel.Unique {
			if len(u) == len(want) {
				match := true
				for i := range want {
					if u[i] != want[i] {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
		}
		return false
	}
	if !hasUnique("email") {
		t.Errorf("unique sets %v missing [email]", rel.Unique)
	}
	if !hasUnique("slug", "tenant") {
		t.Errorf("unique sets %v missing [slug tenant]", rel.Unique)
	}
	for _, u := range rel.Unique {
		if len(u) == 1 && u[0] == "id" {
			t.Errorf("unique sets %v include the primary key, want it excluded", rel.Unique)
		}
	}

	// P16: table, column, and schema comments are populated.
	if rel.Comment != "People records" {
		t.Errorf("table comment = %q, want %q", rel.Comment, "People records")
	}
	emailCol, _ := rel.Column("email")
	if emailCol.Comment != "Primary contact email" {
		t.Errorf("email comment = %q, want %q", emailCol.Comment, "Primary contact email")
	}
	if got := model.SchemaComment("public"); got != "The default schema" {
		t.Errorf("schema comment = %q, want %q", got, "The default schema")
	}
}

// TestIntegrationVoidCallStatus proves a void-returning function answers 204 on
// both verbs, not just POST. A STABLE void function runs through the read path
// (executeCallRead); before the fix that path never detected void, so a GET
// answered 200 with a body while a POST to the same function answered 204. Both
// now signal 204. Finding 03-P17.
func TestIntegrationVoidCallStatus(t *testing.T) {
	be := openBE(t)
	ctx := context.Background()

	if _, err := be.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION public._dbrest_void_stable() RETURNS void
			LANGUAGE sql STABLE AS $$ SELECT $$;
		CREATE OR REPLACE FUNCTION public._dbrest_void_volatile() RETURNS void
			LANGUAGE sql VOLATILE AS $$ SELECT $$`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS public._dbrest_void_stable();
			DROP FUNCTION IF EXISTS public._dbrest_void_volatile()`)
	})

	status := func(fn, method string) int {
		t.Helper()
		plan := &ir.Plan{
			ReadOnly: method == "GET",
			Call:     &ir.Call{Function: ir.Ref{Name: fn}, Args: map[string]ir.Value{}},
		}
		res, err := be.Execute(ctx, plan, &reqctx.Context{Method: method, Path: "/rpc/" + fn})
		if err != nil {
			t.Fatalf("Execute(%s %s): %v", method, fn, err)
		}
		return res.ResponseControls().Status
	}

	// GET to the stable function runs the read path; POST to the volatile function
	// runs the write path. Both detect void and signal 204.
	if got := status("_dbrest_void_stable", "GET"); got != 204 {
		t.Errorf("GET void status = %d, want 204 (read path void detection)", got)
	}
	if got := status("_dbrest_void_volatile", "POST"); got != 204 {
		t.Errorf("POST void status = %d, want 204", got)
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
