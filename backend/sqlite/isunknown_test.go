package sqlite

import (
	"context"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
)

// openFlags seeds a table with a boolean-style column (SQLite stores 0/1/NULL)
// and a text column that literally holds the word "true", so the type-driven
// coercion of item 07.4 can be exercised against a real engine.
func openFlags(t *testing.T) *Backend {
	t.Helper()
	b := openSeeded(t)
	_, err := b.DB().Exec(`
		CREATE TABLE flags (
			id    INTEGER PRIMARY KEY,
			done  INTEGER,
			label TEXT
		);
		INSERT INTO flags (id, done, label) VALUES
			(1, 1,    'true'),
			(2, 0,    'false'),
			(3, NULL, 'unset');
	`)
	if err != nil {
		t.Fatalf("seed flags: %v", err)
	}
	return b
}

func flagIDs(t *testing.T, b *Backend, where ir.Cond) []any {
	t.Helper()
	q := &ir.Query{
		Relation: ir.Ref{Name: "flags"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"id"}}},
		Order:    []ir.OrderTerm{{Path: []string{"id"}}},
		Where:    &where,
	}
	plan := &ir.Plan{Query: q, ReadOnly: true}
	rc := &reqctx.Context{Role: "anon", Method: "GET"}
	res, err := b.Execute(context.Background(), plan, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rows := readAll(t, res)
	ids := make([]any, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r["id"])
	}
	return ids
}

// 07.4 task 1: is.unknown matches the NULL row through the "col IS NULL"
// fallback SQLite uses.
func TestSQLiteIsUnknownMatchesNull(t *testing.T) {
	b := openFlags(t)
	ids := flagIDs(t, b, ir.Compare{Path: []string{"done"}, Op: ir.OpIs, Value: ir.Value{Text: "unknown"}})
	if len(ids) != 1 || ids[0].(int64) != 3 {
		t.Errorf("is.unknown matched %v, want [3]", ids)
	}
}

// 07.4 task 2: eq.true against the boolean column matches the 1 row.
func TestSQLiteEqTrueBooleanColumn(t *testing.T) {
	b := openFlags(t)
	ids := flagIDs(t, b, ir.Compare{
		Path: []string{"done"}, Op: ir.OpEq, ColumnType: "bool", Value: ir.Value{Text: "true"},
	})
	if len(ids) != 1 || ids[0].(int64) != 1 {
		t.Errorf("done=eq.true matched %v, want [1]", ids)
	}
}

// 07.4 task 2: eq.true against the text column matches the row literally holding
// the word "true", not a coerced boolean.
func TestSQLiteEqTrueTextColumn(t *testing.T) {
	b := openFlags(t)
	ids := flagIDs(t, b, ir.Compare{
		Path: []string{"label"}, Op: ir.OpEq, ColumnType: "text", Value: ir.Value{Text: "true"},
	})
	if len(ids) != 1 || ids[0].(int64) != 1 {
		t.Errorf("label=eq.true matched %v, want [1] (the row holding the word)", ids)
	}
}
