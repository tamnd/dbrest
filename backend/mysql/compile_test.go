package mysql

import (
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// These tests drive the shared compiler (backend/sqlgen) with the real MySQL
// Dialect over fixed plans and snapshot the emitted statement. This is the
// database-free verification spec 06 section 7 prescribes: the dialect and the
// SQL it produces are checked together, with no live server. It also proves the
// dialect satisfies the whole Dialect interface the compiler calls, not just the
// methods a unit test exercises in isolation.

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
	// MySQL has no NULLS keyword: NULL placement rides in an explicit IS NULL sort
	// key ahead of the column term, and the placeholders are positional ?.
	want := "SELECT `title`, `year` FROM `public`.`films` " +
		"WHERE (`year` >= ? AND `rating` = ?) " +
		"ORDER BY (`title` IS NULL) DESC, `title` DESC LIMIT 10"
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
	// text folds onto CHAR, the MySQL CAST target; there is no AS TEXT.
	want := "SELECT CAST(`year` AS CHAR) AS `y` FROM `films`"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileOffsetWithoutLimitSnapshot(t *testing.T) {
	offset := 20
	st, err := sqlgen.CompileRead(d, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Offset:   &offset,
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	// MySQL cannot take OFFSET without LIMIT; the max-bigint idiom stands in.
	want := "SELECT * FROM `films` LIMIT 18446744073709551615 OFFSET 20"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpsertMergeSnapshot(t *testing.T) {
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
	// The conflict target is dropped: ON DUPLICATE KEY fires on any unique key.
	want := "INSERT INTO `players` (`id`, `name`) VALUES (?, ?) " +
		"ON DUPLICATE KEY UPDATE `id` = VALUES(`id`), `name` = VALUES(`name`)"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpsertIgnoreSnapshot(t *testing.T) {
	st, err := sqlgen.CompileInsert(d, &ir.Query{
		Kind:     ir.Upsert,
		Relation: ir.Ref{Name: "players"},
		Write: &ir.WriteSpec{
			Columns:  []string{"id", "name"},
			Rows:     []map[string]ir.Value{{"id": {Text: "7"}, "name": {Text: "Aria"}}},
			Conflict: &ir.Conflict{Target: []string{"id"}, Resolution: ir.ConflictIgnore},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	// Ignore is a no-op self-assignment over the first column, which suppresses
	// only the duplicate-key error.
	want := "INSERT INTO `players` (`id`, `name`) VALUES (?, ?) " +
		"ON DUPLICATE KEY UPDATE `id` = `id`"
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
	want := "SELECT * FROM `films` WHERE REGEXP_LIKE(`title`, ?, 'i')"
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
	want := "SELECT * FROM `docs` WHERE MATCH(`body`) AGAINST(? IN BOOLEAN MODE)"
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	// The web grammar is translated to boolean mode before binding: both bare
	// terms are required.
	if len(st.Args) != 1 || st.Args[0] != "+cat +dog" {
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
