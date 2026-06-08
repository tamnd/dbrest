package sqlserver

import (
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// These tests drive the shared compiler (backend/sqlgen) with the real SQL
// Server Dialect over fixed plans and snapshot the emitted statement. This is
// the database-free verification spec 06 section 7 prescribes. The read path
// exercises every read-side seam (bracket quoting, named @pN, the CASE NULL sort
// key, and OFFSET/FETCH with the injected ORDER BY); the write-statement
// assembly that positions OUTPUT and drives the multi-statement upsert is the
// data-plane slice's, so those are checked at the fragment level and asserted as
// the documented deferral here.

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
	want := "SELECT [title], [year] FROM [public].[films] " +
		"WHERE ([year] >= @p1 AND [rating] = @p2) " +
		"ORDER BY CASE WHEN [title] IS NULL THEN 0 ELSE 1 END, [title] DESC " +
		"OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY"
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
	want := "SELECT CAST([year] AS NVARCHAR(MAX)) AS [y] FROM [films]"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileOffsetInjectsOrderSnapshot(t *testing.T) {
	offset := 20
	st, err := sqlgen.CompileRead(d, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Offset:   &offset,
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	// OFFSET/FETCH needs an ORDER BY; with none from the client the dialect injects
	// the constant order so paging stays valid.
	want := "SELECT * FROM [films] ORDER BY (SELECT 1) OFFSET 20 ROWS"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileInsertSnapshot(t *testing.T) {
	st, err := sqlgen.CompileInsert(d, &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "authors"},
		Write: &ir.WriteSpec{
			Columns: []string{"name"},
			Rows:    []map[string]ir.Value{{"name": {Text: "Borges"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	// A plain insert with no row-return fits the shared assembler; OUTPUT placement
	// is a separate concern exercised only when columns are requested back.
	want := "INSERT INTO [authors] ([name]) VALUES (@p1)"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpsertIsDeferred(t *testing.T) {
	_, err := sqlgen.CompileInsert(d, &ir.Query{
		Kind:     ir.Upsert,
		Relation: ir.Ref{Name: "players"},
		Write: &ir.WriteSpec{
			Columns:  []string{"id", "name"},
			Rows:     []map[string]ir.Value{{"id": {Text: "7"}, "name": {Text: "Aria"}}},
			Conflict: &ir.Conflict{Target: []string{"id"}, Resolution: ir.ConflictMerge},
		},
	}, nil)
	// The single-statement compiler cannot build a SQL Server upsert; it surfaces
	// the dialect's error rather than emitting MERGE or a wrong clause.
	if err == nil {
		t.Error("a SQL Server upsert through the single-statement compiler should error")
	}
}

func TestCompileRegexSnapshot(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"title"}, Op: ir.OpIMatch, Value: ir.Value{Text: "^bl"}})
	st, err := sqlgen.CompileRead(d, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	// This is the modern-server path; on a stock server the capability gate raises
	// PGRST127 before the compiler is reached.
	want := "SELECT * FROM [films] WHERE REGEXP_LIKE([title], @p1, 'i')"
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
	want := "SELECT * FROM [docs] WHERE CONTAINS([body], @p1)"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	// The web grammar is translated to a CONTAINS condition before binding.
	if len(st.Args) != 1 || st.Args[0] != `"cat" AND "dog"` {
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
