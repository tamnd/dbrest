package sqlgen

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// stub is a minimal PostgreSQL-flavored dialect for exercising the compiler
// without pulling in a real backend (which would import this package).
type stub struct{}

func (stub) QuoteIdent(n string) string { return `"` + n + `"` }
func (stub) Placeholder(n int) string   { return "$" + strconv.Itoa(n) }
func (stub) LimitOffset(limit, offset *int, _ bool) string {
	s := ""
	if limit != nil {
		s += "LIMIT " + strconv.Itoa(*limit)
	}
	if offset != nil {
		if s != "" {
			s += " "
		}
		s += "OFFSET " + strconv.Itoa(*offset)
	}
	return s
}
func (stub) NullsOrder(col, dir string, desc bool, nf *bool) (string, string) {
	first := desc
	if nf != nil {
		first = *nf
	}
	nulls := "NULLS LAST"
	if first {
		nulls = "NULLS FIRST"
	}
	return "", col + " " + dir + " " + nulls
}
func (stub) Returning(c []string) (string, bool) {
	if len(c) == 0 {
		return "", false
	}
	return "RETURNING " + strings.Join(c, ", "), true
}
func (stub) Upsert(s UpsertSpec) (string, error) {
	out := "ON CONFLICT"
	if len(s.Target) > 0 {
		out += " (" + strings.Join(s.Target, ", ") + ")"
	}
	if s.Ignore || len(s.Update) == 0 {
		return out + " DO NOTHING", nil
	}
	sets := make([]string, len(s.Update))
	for i, c := range s.Update {
		sets[i] = c + " = excluded." + c
	}
	return out + " DO UPDATE SET " + strings.Join(sets, ", "), nil
}
func (stub) JSONObject([]Pair) string                  { return "" }
func (stub) JSONAgg(e, o string) string                { return "" }
func (stub) Cast(e, t string) string                   { return "CAST(" + e + " AS " + t + ")" }
func (stub) Regex(e, p string, ci bool) (string, bool) { return e + " ~ " + PatternMark, true }
func (stub) SessionRead(k string) string               { return "" }
func (stub) SessionWrite(k string) (string, bool)      { return "", false }
func (stub) BoolValue(v bool) string {
	if v {
		return "TRUE"
	}
	return "FALSE"
}

func compile(t *testing.T, q *ir.Query) *Statement {
	t.Helper()
	st, err := CompileRead(stub{}, q)
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	return st
}

func col(name string) ir.Column { return ir.Column{Path: []string{name}} }

func TestCompileSelectStar(t *testing.T) {
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "films"}})
	if st.SQL != `SELECT * FROM "films"` {
		t.Errorf("SQL = %q", st.SQL)
	}
	if len(st.Args) != 0 {
		t.Errorf("Args = %v, want none", st.Args)
	}
}

func TestCompileColumnsSchemaQualified(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{col("id"), col("title")},
	})
	want := `SELECT "id", "title" FROM "public"."films"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileAliasAndCast(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"year"}, Cast: "text", Alias: "y"}},
	})
	want := `SELECT CAST("year" AS text) AS "y" FROM "films"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileFiltersAndedBound(t *testing.T) {
	where := ir.Cond(ir.And{Kids: []ir.Cond{
		ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}},
		ir.Compare{Path: []string{"rating"}, Op: ir.OpEq, Value: ir.Value{Text: "PG"}},
	}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	want := `SELECT * FROM "films" WHERE ("year" >= $1 AND "rating" = $2)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 2 || st.Args[0] != "2000" || st.Args[1] != "PG" {
		t.Errorf("Args = %v", st.Args)
	}
}

func TestCompileInList(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpIn, Value: ir.Value{List: []string{"1", "2", "3"}}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE "id" IN ($1, $2, $3)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 3 {
		t.Errorf("Args = %v", st.Args)
	}
}

func TestCompileIsNullAndBool(t *testing.T) {
	cases := map[string]string{
		"null":     `"c" IS NULL`,
		"not_null": `"c" IS NOT NULL`,
		"true":     `"c" IS TRUE`,
		"false":    `"c" IS FALSE`,
	}
	for in, want := range cases {
		where := ir.Cond(ir.Compare{Path: []string{"c"}, Op: ir.OpIs, Value: ir.Value{Text: in}})
		st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
		wantSQL := `SELECT * FROM "t" WHERE ` + want
		if st.SQL != wantSQL {
			t.Errorf("is.%s: SQL = %q, want %q", in, st.SQL, wantSQL)
		}
	}
}

func TestCompileNegate(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"c"}, Op: ir.OpEq, Value: ir.Value{Text: "x"}, Negate: true})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE NOT ("c" = $1)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileRegexUsesPatternMark(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"c"}, Op: ir.OpMatch, Value: ir.Value{Text: "^a"}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE "c" ~ $1`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 1 || st.Args[0] != "^a" {
		t.Errorf("Args = %v", st.Args)
	}
}

func TestCompileOrderNullsAndPaging(t *testing.T) {
	limit, offset := 10, 5
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"title"}, Desc: true}},
		Limit:    &limit,
		Offset:   &offset,
	})
	want := `SELECT * FROM "films" ORDER BY "title" DESC NULLS FIRST LIMIT 10 OFFSET 5`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileEmptyInMatchesNothing(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpIn, Value: ir.Value{List: nil}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if st.SQL != `SELECT * FROM "t" WHERE 1 = 0` {
		t.Errorf("SQL = %q", st.SQL)
	}
}

func TestCompileCount(t *testing.T) {
	st, err := CompileCount(stub{}, &ir.Query{Relation: ir.Ref{Name: "films"}})
	if err != nil {
		t.Fatalf("CompileCount: %v", err)
	}
	if st.SQL != `SELECT count(*) FROM "films"` {
		t.Errorf("SQL = %q", st.SQL)
	}
}

func TestCompileCountWithFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}})
	st, err := CompileCount(stub{}, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where})
	if err != nil {
		t.Fatalf("CompileCount: %v", err)
	}
	want := `SELECT count(*) FROM "films" WHERE "year" >= $1`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 1 || st.Args[0] != "2000" {
		t.Errorf("Args = %v", st.Args)
	}
}

func TestCompileAggregateRejected(t *testing.T) {
	_, err := CompileRead(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Select:   []ir.SelectItem{ir.Aggregate{Func: ir.AggCount}},
	})
	if err == nil || err.Code != "PGRST127" {
		t.Fatalf("want PGRST127 for aggregate, got %v", err)
	}
}
