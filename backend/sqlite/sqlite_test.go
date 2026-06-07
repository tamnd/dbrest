package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
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
