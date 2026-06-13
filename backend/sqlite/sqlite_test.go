package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// openSeeded returns an in-memory SQLite backend with a films table populated
// for the read-path tests. A shared-cache memory DSN keeps the single pooled
// connection alive for the test's lifetime.
func openSeeded(t *testing.T) *Backend {
	t.Helper()
	// A uniquely named shared-cache memory DB isolates each test: connections in
	// one pool share the database, but a different name is a different database.
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	_, err = b.DB().Exec(`
		CREATE TABLE films (
			id      INTEGER PRIMARY KEY,
			title   TEXT NOT NULL,
			year    INTEGER,
			rating  TEXT DEFAULT 'NR'
		);
		INSERT INTO films (id, title, year, rating) VALUES
			(1, 'Metropolis', 1927, 'NR'),
			(2, 'Blade Runner', 1982, 'R'),
			(3, 'Arrival', 2016, 'PG13'),
			(4, 'Untitled', NULL, 'NR');
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return b
}

func readAll(t *testing.T, res backend.Result) []map[string]any {
	t.Helper()
	rs := res.Rows()
	defer rs.Close()
	cols := rs.Columns()
	var out []map[string]any
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			t.Fatalf("Values: %v", err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}

func execRead(t *testing.T, b *Backend, q *ir.Query) []map[string]any {
	t.Helper()
	plan := &ir.Plan{Query: q, ReadOnly: true}
	rc := &reqctx.Context{Role: "anon", Method: "GET"}
	res, err := b.Execute(context.Background(), plan, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return readAll(t, res)
}

func TestIntrospect(t *testing.T) {
	b := openSeeded(t)
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	rel, ok := model.Lookup("films", nil)
	if !ok {
		t.Fatal("films not found in model")
	}
	if len(rel.PrimaryKey) != 1 || rel.PrimaryKey[0] != "id" {
		t.Errorf("PrimaryKey = %v, want [id]", rel.PrimaryKey)
	}
	title, ok := rel.Column("title")
	if !ok {
		t.Fatal("title column missing")
	}
	if title.Nullable {
		t.Error("title is NOT NULL, should not be Nullable")
	}
	if title.Type != "text" {
		t.Errorf("title.Type = %q, want text", title.Type)
	}
	rating, _ := rel.Column("rating")
	if !rating.HasDefault {
		t.Error("rating has a DEFAULT, HasDefault should be true")
	}
}

func TestExecuteSelectColumns(t *testing.T) {
	b := openSeeded(t)
	rows := execRead(t, b, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"id"}}, ir.Column{Path: []string{"title"}}},
		Order:    []ir.OrderTerm{{Path: []string{"id"}}},
	})
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	if rows[0]["title"] != "Metropolis" {
		t.Errorf("row0 title = %v", rows[0]["title"])
	}
	if _, hasYear := rows[0]["year"]; hasYear {
		t.Error("year should not be projected")
	}
}

func TestExecuteFilterAndOrder(t *testing.T) {
	b := openSeeded(t)
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "1980"}})
	rows := execRead(t, b, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Order:    []ir.OrderTerm{{Path: []string{"year"}, Desc: true}},
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["title"] != "Arrival" {
		t.Errorf("first by year desc = %v, want Arrival", rows[0]["title"])
	}
}

func TestExecuteInList(t *testing.T) {
	b := openSeeded(t)
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpIn, Value: ir.Value{List: []string{"1", "3"}}})
	rows := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

func TestExecuteIsNull(t *testing.T) {
	b := openSeeded(t)
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpIs, Value: ir.Value{Text: "null"}})
	rows := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	if len(rows) != 1 || rows[0]["title"] != "Untitled" {
		t.Fatalf("is.null got %v", rows)
	}
}

func TestExecuteRegexMatch(t *testing.T) {
	b := openSeeded(t)
	where := ir.Cond(ir.Compare{Path: []string{"title"}, Op: ir.OpMatch, Value: ir.Value{Text: "^A"}})
	rows := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	if len(rows) != 1 || rows[0]["title"] != "Arrival" {
		t.Fatalf("regex match got %v", rows)
	}
}

func TestExecuteLimitOffset(t *testing.T) {
	b := openSeeded(t)
	limit, offset := 2, 1
	rows := execRead(t, b, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"id"}}},
		Limit:    &limit,
		Offset:   &offset,
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["id"].(int64) != 2 {
		t.Errorf("first id = %v, want 2", rows[0]["id"])
	}
}

func TestExecuteExactCount(t *testing.T) {
	b := openSeeded(t)
	limit := 2
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "1980"}})
	plan := &ir.Plan{Query: &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Limit:    &limit,
		Count:    ir.CountExact,
	}, ReadOnly: true}
	res, err := b.Execute(context.Background(), plan, &reqctx.Context{Role: "anon"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	total, ok := res.Count()
	if !ok {
		t.Fatal("Count() should report a total when count=exact")
	}
	// Two films have year >= 1980 (Blade Runner 1982, Arrival 2016).
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	rows := readAll(t, res)
	if len(rows) != 2 {
		t.Errorf("windowed rows = %d, want 2", len(rows))
	}
}

func TestExecuteNoCountByDefault(t *testing.T) {
	b := openSeeded(t)
	plan := &ir.Plan{Query: &ir.Query{Relation: ir.Ref{Name: "films"}}, ReadOnly: true}
	res, err := b.Execute(context.Background(), plan, &reqctx.Context{Role: "anon"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, ok := res.Count(); ok {
		t.Error("Count() should report no total when no count was requested")
	}
}

// execWrite runs a write plan and returns the result for inspection.
func execWrite(t *testing.T, b *Backend, q *ir.Query) backend.Result {
	t.Helper()
	rel, ok := mustModel(t, b).Lookup("films", nil)
	if !ok {
		t.Fatal("films not in model")
	}
	// Resolve the relation reference the way the planner does.
	q.Relation = ir.Ref{Name: rel.Name}
	pl := &ir.Plan{Query: q, Rel: rel}
	if q.Write != nil && q.Write.Conflict != nil && len(q.Write.Conflict.Target) == 0 {
		q.Write.Conflict.Target = rel.PrimaryKey
	}
	res, err := b.Execute(context.Background(), pl, &reqctx.Context{Role: "anon"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func mustModel(t *testing.T, b *Backend) *schema.Model {
	t.Helper()
	m, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	return m
}

func TestExecuteInsertRepresentation(t *testing.T) {
	b := openSeeded(t)
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Return:  ir.ReturnRepresentation,
			Columns: []string{"id", "title", "year"},
			Rows:    []map[string]ir.Value{{"id": ir.Value{JSON: json.Number("5")}, "title": ir.Value{JSON: "Dune"}, "year": ir.Value{JSON: json.Number("2021")}}},
		},
	})
	rows := readAll(t, res)
	if len(rows) != 1 || rows[0]["title"] != "Dune" || rows[0]["id"].(int64) != 5 {
		t.Fatalf("insert representation = %v", rows)
	}
	if n, ok := res.Affected(); !ok || n != 1 {
		t.Errorf("Affected = %d,%v want 1,true", n, ok)
	}
	// The row is committed: a follow-up read sees it.
	all := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}})
	if len(all) != 5 {
		t.Errorf("after insert, films count = %d, want 5", len(all))
	}
}

func TestExecuteInsertMinimalReturnsPK(t *testing.T) {
	b := openSeeded(t)
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Return:  ir.ReturnMinimal,
			Columns: []string{"id", "title"},
			Rows:    []map[string]ir.Value{{"id": ir.Value{JSON: json.Number("7")}, "title": ir.Value{JSON: "Tenet"}}},
		},
	})
	// Minimal still returns the primary key so the handler can build Location.
	rows := readAll(t, res)
	if len(rows) != 1 || rows[0]["id"].(int64) != 7 {
		t.Fatalf("minimal insert pk = %v", rows)
	}
}

func TestExecuteUpdate(t *testing.T) {
	b := openSeeded(t)
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "2"}})
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Update,
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Write: &ir.WriteSpec{
			Return: ir.ReturnRepresentation,
			Set:    map[string]ir.Value{"rating": {JSON: "PG"}},
		},
	})
	rows := readAll(t, res)
	if len(rows) != 1 || rows[0]["rating"] != "PG" {
		t.Fatalf("update representation = %v", rows)
	}
}

func TestExecuteUpdateMinimalAffected(t *testing.T) {
	b := openSeeded(t)
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Update,
		Relation: ir.Ref{Name: "films"},
		Write:    &ir.WriteSpec{Return: ir.ReturnMinimal, Set: map[string]ir.Value{"rating": {JSON: "X"}}},
	})
	// No filter updates every row; minimal reports the affected count.
	if n, ok := res.Affected(); !ok || n != 4 {
		t.Errorf("Affected = %d,%v want 4,true", n, ok)
	}
	if rows := readAll(t, res); len(rows) != 0 {
		t.Errorf("minimal update should buffer no rows, got %v", rows)
	}
}

func TestExecuteDelete(t *testing.T) {
	b := openSeeded(t)
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}})
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Delete,
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Write:    &ir.WriteSpec{Return: ir.ReturnMinimal},
	})
	if n, _ := res.Affected(); n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	if all := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}}); len(all) != 3 {
		t.Errorf("after delete, count = %d, want 3", len(all))
	}
}

func TestExecuteUpsertMergeUpdatesExisting(t *testing.T) {
	b := openSeeded(t)
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Upsert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Return:   ir.ReturnRepresentation,
			Columns:  []string{"id", "title"},
			Rows:     []map[string]ir.Value{{"id": ir.Value{JSON: json.Number("1")}, "title": ir.Value{JSON: "Metropolis (restored)"}}},
			Conflict: &ir.Conflict{Resolution: ir.ConflictMerge}, // target defaults to PK
		},
	})
	rows := readAll(t, res)
	if len(rows) != 1 || rows[0]["title"] != "Metropolis (restored)" {
		t.Fatalf("upsert merge = %v", rows)
	}
	if all := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}}); len(all) != 4 {
		t.Errorf("upsert of existing id should not add a row, count = %d", len(all))
	}
}

func TestExecuteTxRollbackDoesNotPersist(t *testing.T) {
	b := openSeeded(t)
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Return:  ir.ReturnRepresentation,
			Tx:      ir.TxRollback,
			Columns: []string{"id", "title"},
			Rows:    []map[string]ir.Value{{"id": ir.Value{JSON: json.Number("9")}, "title": ir.Value{JSON: "Ghost"}}},
		},
	})
	// The representation reflects the would-be row ...
	if rows := readAll(t, res); len(rows) != 1 {
		t.Fatalf("rollback representation = %v", rows)
	}
	// ... but nothing is persisted.
	if all := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}}); len(all) != 4 {
		t.Errorf("after rollback, count = %d, want 4", len(all))
	}
}

func TestMapErrorUniqueViolation(t *testing.T) {
	b := openSeeded(t)
	// Inserting a duplicate primary key trips a constraint mapped to 23505/409.
	pl := &ir.Plan{Query: &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Return:  ir.ReturnMinimal,
			Columns: []string{"id", "title"},
			Rows:    []map[string]ir.Value{{"id": ir.Value{JSON: json.Number("1")}, "title": ir.Value{JSON: "Dup"}}},
		},
	}}
	rel, _ := mustModel(t, b).Lookup("films", nil)
	pl.Rel = rel
	_, err := b.Execute(context.Background(), pl, &reqctx.Context{Role: "anon"})
	if err == nil {
		t.Fatal("want a constraint error")
	}
	api := pgerr.As(err)
	if api == nil || api.Code != pgerr.CodeUniqueViolation || api.HTTPStatus != 409 {
		t.Fatalf("err = %#v, want 23505/409", api)
	}
	// The message is PostgreSQL's wording, not SQLite's native text, and the
	// native "UNIQUE constraint failed" string never leaks into details.
	if api.Message != "duplicate key value violates unique constraint" {
		t.Errorf("message = %q, want PG unique wording", api.Message)
	}
	if api.Details != nil {
		t.Errorf("details = %q, want no leaked native text", *api.Details)
	}
}

// A NOT NULL violation reconstructs PostgreSQL's exact wording from the
// relation and column SQLite names in its error text.
func TestMapErrorNotNullViolation(t *testing.T) {
	b := openSeeded(t)
	pl := &ir.Plan{Query: &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Return:  ir.ReturnMinimal,
			Columns: []string{"id", "title"},
			Rows:    []map[string]ir.Value{{"id": ir.Value{JSON: json.Number("9")}, "title": ir.Value{JSON: nil}}},
		},
	}}
	rel, _ := mustModel(t, b).Lookup("films", nil)
	pl.Rel = rel
	_, err := b.Execute(context.Background(), pl, &reqctx.Context{Role: "anon"})
	if err == nil {
		t.Fatal("want a constraint error")
	}
	api := pgerr.As(err)
	if api == nil || api.Code != pgerr.CodeNotNullViolation || api.HTTPStatus != 400 {
		t.Fatalf("err = %#v, want 23502/400", api)
	}
	want := `null value in column "title" of relation "films" violates not-null constraint`
	if api.Message != want {
		t.Errorf("message = %q, want %q", api.Message, want)
	}
	if api.Details != nil {
		t.Errorf("details = %q, want no leaked native text", *api.Details)
	}
}

// The synthesis helpers reconstruct PG wording from SQLite's text directly,
// including the constraint name for a CHECK and a graceful fallback when the
// text does not parse.
func TestConstraintMessageSynthesis(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"notnull", notNullMessage("NOT NULL constraint failed: films.title"),
			`null value in column "title" of relation "films" violates not-null constraint`},
		{"notnull-unparsed", notNullMessage("garbage"),
			"null value violates not-null constraint"},
		{"check-named", checkMessage("CHECK constraint failed: rating_valid"),
			`new row violates check constraint "rating_valid"`},
		{"check-bare", checkMessage("CHECK constraint failed: "),
			"new row violates check constraint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
}

func TestExecuteNullsOrdering(t *testing.T) {
	b := openSeeded(t)
	// ASC default puts NULLs last (PG semantics); the NULL-year row sorts last.
	rows := execRead(t, b, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"year"}}},
	})
	if rows[len(rows)-1]["title"] != "Untitled" {
		t.Errorf("NULL year should sort last on ASC, got last = %v", rows[len(rows)-1]["title"])
	}
}
