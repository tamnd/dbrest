package postgres

import (
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// These tests drive the shared compiler (backend/sqlgen) with the real
// PostgreSQL Dialect over fixed plans and snapshot the emitted statement. This
// is the database-free verification spec 06 section 7 prescribes: the dialect
// and the SQL it produces are checked together, with no live server. It also
// proves the dialect satisfies the full Dialect interface the compiler calls,
// not just the methods a unit test exercises in isolation.

func col(name string) ir.Column { return ir.Column{Path: []string{name}} }

func TestCompileReadSnapshot(t *testing.T) {
	where := ir.Cond(ir.And{Kids: []ir.Cond{
		ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}},
		ir.Compare{Path: []string{"rating"}, Op: ir.OpEq, Value: ir.Value{Text: "PG"}},
	}})
	limit := 10
	st, err := sqlgen.CompileRead(d, &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{col("title"), col("year")},
		Where:    &where,
		Order:    []ir.OrderTerm{{Path: []string{"title"}, Desc: true}},
		Limit:    &limit,
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	want := `SELECT "title", "year" FROM "public"."films" ` +
		`WHERE ("year" >= $1 AND "rating" = $2) ` +
		`ORDER BY "title" DESC NULLS FIRST LIMIT 10`
	if st.SQL != want {
		t.Errorf("SQL =\n  %q\nwant\n  %q", st.SQL, want)
	}
	if len(st.Args) != 2 || st.Args[0] != "2000" || st.Args[1] != "PG" {
		t.Errorf("Args = %v", st.Args)
	}
}

func TestCompileReadCastSnapshot(t *testing.T) {
	st, err := sqlgen.CompileRead(d, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"year"}, Cast: "text", Alias: "y"}},
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	want := `SELECT ("year")::text AS "y" FROM "films"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileInsertReturningSnapshot(t *testing.T) {
	st, err := sqlgen.CompileInsert(d, &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "authors"},
		Write: &ir.WriteSpec{
			Columns: []string{"name"},
			Rows:    []map[string]ir.Value{{"name": {Text: "Borges"}}},
		},
	}, []string{"id", "name"})
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "authors" ("name") VALUES ($1) RETURNING "id", "name"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpsertSnapshot(t *testing.T) {
	st, err := sqlgen.CompileInsert(d, &ir.Query{
		Kind:     ir.Upsert,
		Relation: ir.Ref{Name: "players"},
		Write: &ir.WriteSpec{
			Columns:  []string{"id", "name"},
			Rows:     []map[string]ir.Value{{"id": {Text: "7"}, "name": {Text: "Aria"}}},
			Conflict: &ir.Conflict{Target: []string{"id"}, Resolution: ir.ConflictMerge},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "players" ("id", "name") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "id" = excluded."id", "name" = excluded."name"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileRegexSnapshot(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"title"}, Op: ir.OpIMatch, Value: ir.Value{Text: "^bl"}})
	st, err := sqlgen.CompileRead(d, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	want := `SELECT * FROM "films" WHERE "title" ~* $1`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 1 || st.Args[0] != "^bl" {
		t.Errorf("Args = %v", st.Args)
	}
}

func TestCompileFTSSnapshot(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"body"}, Op: ir.OpFTS, FTS: ir.FTSWeb, Value: ir.Value{Text: "cat dog"}})
	st, err := sqlgen.CompileRead(d, &ir.Query{Relation: ir.Ref{Name: "docs"}, Where: &where})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	want := `SELECT * FROM "docs" WHERE to_tsvector("body") @@ websearch_to_tsquery($1)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	// The raw query text is bound; PostgreSQL parses the web grammar itself.
	if len(st.Args) != 1 || st.Args[0] != "cat dog" {
		t.Errorf("Args = %v", st.Args)
	}
}

func BenchmarkCompileRead(b *testing.B) {
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}})
	limit := 25
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{col("id"), col("title"), col("year")},
		Where:    &where,
		Order:    []ir.OrderTerm{{Path: []string{"title"}}},
		Limit:    &limit,
	}
	for b.Loop() {
		if _, err := sqlgen.CompileRead(d, q); err != nil {
			b.Fatal(err)
		}
	}
}
