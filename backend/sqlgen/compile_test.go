package sqlgen

import (
	"encoding/json"
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
func (stub) RegexFeatureGap(string) string             { return "" }

// FullText models a PostgreSQL-flavored, column-agnostic full text: the index is
// ignored (tsvector works on any column), so a nil idx is fine.
func (stub) FullText(col, _ string, _ *FullTextRef, v ir.FTSVariant, _, _ string) (string, string, bool) {
	ctor := map[ir.FTSVariant]string{
		ir.FTSPlain: "to_tsquery", ir.FTSPlainText: "plainto_tsquery",
		ir.FTSPhrase: "phraseto_tsquery", ir.FTSWeb: "websearch_to_tsquery",
	}[v]
	return "to_tsvector(" + col + ") @@ " + ctor + "(" + PatternMark + ")", "", true
}
func (stub) SessionRead(k string) string          { return "" }
func (stub) SessionWrite(k string) (string, bool) { return "", false }
func (stub) ArrayOp(col, op, val, _ string) (string, bool) {
	return col + " " + op + " " + val, true
}
func (stub) RangeOp(col, op, val string) (string, bool) {
	return col + " " + op + " " + val, true
}
func (stub) ArrayLiteral(s string) string         { return s }
func (stub) InList(_ string) (string, bool)       { return "", false }
func (stub) ArrayArg(e []any, _ string) any       { return JSONArrayArg(e) }
func (stub) ILike(col, val string) (string, bool) { return col + " ILIKE " + val, true }
func (stub) BoolValue(v bool) string {
	if v {
		return "TRUE"
	}
	return "FALSE"
}
func (stub) IsBool(string, bool) (string, bool) { return "", false }
func (stub) IsUnknown(string) (string, bool)    { return "", false }

// JSONPath mirrors the PostgreSQL native ->/->> chain so the shared compiler's
// JSON-path routing is assertable without a real engine.
func (stub) JSONPath(base string, hops []string, asText bool) (string, bool) {
	expr := base
	for i, h := range hops {
		op := "->"
		if asText && i == len(hops)-1 {
			op = "->>"
		}
		if IsJSONArrayIndex(h) {
			expr += op + h
		} else {
			expr += op + "'" + h + "'"
		}
	}
	return expr, true
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

// TestCompileFTSColumnAgnostic checks an fts predicate compiles through the
// dialect on a backend with column-agnostic full text (the stub models
// PostgreSQL), where the planner attached no covering index.
func TestCompileFTSColumnAgnostic(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"body"}, Op: ir.OpFTS, FTS: ir.FTSWeb, Value: ir.Value{Text: "cat"}})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "docs"}, Where: &where})
	want := `SELECT * FROM "docs" WHERE to_tsvector("body") @@ websearch_to_tsquery($1)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
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

// TestCompileQuantifiedEqExpandsToOr checks eq(any) over a list fans out into an
// OR of equalities, each value bound (item 01.1).
func TestCompileQuantifiedEqExpandsToOr(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path:  []string{"id"},
		Op:    ir.OpEq,
		Quant: ir.QAny,
		Value: ir.Value{List: []string{"1", "2", "3"}},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE ("id" = $1 OR "id" = $2 OR "id" = $3)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 3 || st.Args[0] != "1" || st.Args[2] != "3" {
		t.Errorf("Args = %v", st.Args)
	}
}

// TestCompileQuantifiedGtExpandsToAnd checks gt(all) fans out into an AND.
func TestCompileQuantifiedGtExpandsToAnd(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path:  []string{"year"},
		Op:    ir.OpGt,
		Quant: ir.QAll,
		Value: ir.Value{List: []string{"1990", "2000"}},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE ("year" > $1 AND "year" > $2)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestCompileQuantifiedMatchUsesDialectRegex checks match(any) routes each
// element through the dialect regex seam (PatternMark replaced by the bind).
func TestCompileQuantifiedMatchUsesDialectRegex(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path:  []string{"c"},
		Op:    ir.OpMatch,
		Quant: ir.QAny,
		Value: ir.Value{List: []string{"^a", "b$"}},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE ("c" ~ $1 OR "c" ~ $2)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 2 || st.Args[0] != "^a" || st.Args[1] != "b$" {
		t.Errorf("Args = %v", st.Args)
	}
}

// TestCompileQuantifiedNegated checks a negated quantified compare wraps the
// whole fan-out in NOT (…).
func TestCompileQuantifiedNegated(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path:   []string{"id"},
		Op:     ir.OpEq,
		Quant:  ir.QAny,
		Negate: true,
		Value:  ir.Value{List: []string{"1", "2"}},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	want := `SELECT * FROM "t" WHERE NOT (("id" = $1 OR "id" = $2))`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
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

// jnum and jstr build payload values the way the parser does: a JSON number is
// carried as json.Number, a string as a plain string.
func jnum(n string) ir.Value { return ir.Value{JSON: json.Number(n)} }
func jstr(s string) ir.Value { return ir.Value{JSON: s} }

func TestCompileInsertSingleRow(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Columns: []string{"title", "year"},
			Rows:    []map[string]ir.Value{{"title": jstr("Dune"), "year": jnum("2021")}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "films" ("title", "year") VALUES ($1, $2)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 2 || st.Args[0] != "Dune" || st.Args[1] != int64(2021) {
		t.Errorf("Args = %#v", st.Args)
	}
}

func TestCompileInsertMultiRow(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Columns: []string{"title"},
			Rows: []map[string]ir.Value{
				{"title": jstr("A")},
				{"title": jstr("B")},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "films" ("title") VALUES ($1), ($2)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileInsertMissingDefaultAndNull(t *testing.T) {
	// A row missing a column takes DEFAULT only under an explicit missing=default ...
	st, _ := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Write: &ir.WriteSpec{
			Columns: []string{"a", "b"},
			Missing: ir.MissingDefault,
			Rows:    []map[string]ir.Value{{"a": jstr("x")}},
		},
	}, nil)
	if st.SQL != `INSERT INTO "t" ("a", "b") VALUES ($1, DEFAULT)` {
		t.Errorf("default: SQL = %q", st.SQL)
	}
	// ... and a bound NULL by default (MissingNull is the zero value, item 01.18).
	st, _ = CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Write: &ir.WriteSpec{
			Columns: []string{"a", "b"},
			Rows:    []map[string]ir.Value{{"a": jstr("x")}},
		},
	}, nil)
	if st.SQL != `INSERT INTO "t" ("a", "b") VALUES ($1, $2)` {
		t.Errorf("null: SQL = %q", st.SQL)
	}
	if len(st.Args) != 2 || st.Args[1] != nil {
		t.Errorf("null: Args = %#v", st.Args)
	}
}

func TestCompileInsertReturning(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Columns: []string{"title"},
			Rows:    []map[string]ir.Value{{"title": jstr("Dune")}},
		},
	}, []string{"id", "title"})
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "films" ("title") VALUES ($1) RETURNING "id", "title"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpsertMerge(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Columns:  []string{"id", "title"},
			Rows:     []map[string]ir.Value{{"id": jnum("1"), "title": jstr("Dune")}},
			Conflict: &ir.Conflict{Target: []string{"id"}, Resolution: ir.ConflictMerge},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "films" ("id", "title") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "id" = excluded."id", "title" = excluded."title"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpsertIgnore(t *testing.T) {
	st, _ := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Columns:  []string{"id"},
			Rows:     []map[string]ir.Value{{"id": jnum("1")}},
			Conflict: &ir.Conflict{Target: []string{"id"}, Resolution: ir.ConflictIgnore},
		},
	}, nil)
	want := `INSERT INTO "films" ("id") VALUES ($1) ON CONFLICT ("id") DO NOTHING`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileUpdate(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}})
	st, err := CompileUpdate(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Write:    &ir.WriteSpec{Set: map[string]ir.Value{"title": jstr("Dune"), "year": jnum("2021")}},
	}, nil)
	if err != nil {
		t.Fatalf("CompileUpdate: %v", err)
	}
	// SET columns are written in sorted order, so title then year, then WHERE.
	want := `UPDATE "films" SET "title" = $1, "year" = $2 WHERE "id" = $3`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 3 || st.Args[0] != "Dune" || st.Args[1] != int64(2021) || st.Args[2] != "1" {
		t.Errorf("Args = %#v", st.Args)
	}
}

func TestCompileUpdateNoFilterTouchesAll(t *testing.T) {
	st, _ := CompileUpdate(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Write:    &ir.WriteSpec{Set: map[string]ir.Value{"a": jstr("x")}},
	}, nil)
	if st.SQL != `UPDATE "t" SET "a" = $1` {
		t.Errorf("SQL = %q", st.SQL)
	}
}

func TestCompileDelete(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "9"}})
	st, err := CompileDelete(stub{}, &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}, []string{"id"})
	if err != nil {
		t.Fatalf("CompileDelete: %v", err)
	}
	want := `DELETE FROM "films" WHERE "id" = $1 RETURNING "id"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestCompileInsertEmptyPayloadRejected(t *testing.T) {
	_, err := CompileInsert(stub{}, &ir.Query{Relation: ir.Ref{Name: "t"}, Write: &ir.WriteSpec{}}, nil)
	if err == nil {
		t.Fatal("want an error for an empty insert payload")
	}
}

// A range operator on an engine whose dialect declines (no range types) reports
// PGRST127 and names the PostgREST token, rather than emitting a quietly
// different predicate. A range-capable dialect lowers it instead; see
// rangeop_test.go.
func TestCompileRangeOperatorRejectedNamed(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"period"}, Op: ir.OpRangeSL, Value: ir.Value{Text: "[1,2)"}})
	_, err := CompileRead(noRangeDialect{}, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if err == nil || err.Code != "PGRST127" {
		t.Fatalf("want PGRST127, got %v", err)
	}
	if err.Details == nil || !strings.Contains(*err.Details, "sl") {
		t.Errorf("details = %v, want it to name the sl operator", err.Details)
	}
}

// TestCompileBareCount renders count() with no grouping column as count(*) over
// the whole relation, keyed to its default response name (item 01.4).
func TestCompileBareCount(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Select:   []ir.SelectItem{ir.Aggregate{Func: ir.AggCount}},
	})
	want := `SELECT count(*) AS "count" FROM "t"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestCompileColumnAggregateGroupsBy renders category, amount.sum() as a grouped
// aggregate: the plain column is the GROUP BY key, the aggregate folds per group.
func TestCompileColumnAggregateGroupsBy(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "sales"},
		Select: []ir.SelectItem{
			col("category"),
			ir.Aggregate{Func: ir.AggSum, Arg: &ir.Column{Path: []string{"amount"}}},
		},
	})
	want := `SELECT "category", sum("amount") AS "sum" FROM "public"."sales" GROUP BY "category"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestCompileAggregateAliasAndCasts honors a response-key alias, an input cast on
// the aggregated column, and an output cast on the result.
func TestCompileAggregateAliasAndCasts(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "sales"},
		Select: []ir.SelectItem{
			ir.Aggregate{
				Func:  ir.AggSum,
				Arg:   &ir.Column{Path: []string{"amount"}, Cast: "numeric"},
				Cast:  "text",
				Alias: "total",
			},
		},
	})
	want := `SELECT CAST(sum(CAST("amount" AS numeric)) AS text) AS "total" FROM "sales"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// A payload array goes through the dialect on the write path: the stub (like
// every engine without array columns) binds the JSON text, never a PostgreSQL
// {a,b} literal. This is what lets a JSON column round-trip ["go","sql"].
func TestCompileUpdateArrayBindsDialectForm(t *testing.T) {
	st, err := CompileUpdate(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "todos"},
		Write: &ir.WriteSpec{Set: map[string]ir.Value{
			"tags": {JSON: []any{"go", "sql"}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("CompileUpdate: %v", err)
	}
	if len(st.Args) != 1 || st.Args[0] != `["go","sql"]` {
		t.Errorf("Args = %#v, want JSON text", st.Args)
	}
}

func TestCompileInsertArrayBindsDialectForm(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "todos"},
		Write: &ir.WriteSpec{
			Columns: []string{"tags"},
			Rows:    []map[string]ir.Value{{"tags": {JSON: []any{json.Number("1"), "two words"}}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	if len(st.Args) != 1 || st.Args[0] != `[1,"two words"]` {
		t.Errorf("Args = %#v, want JSON text", st.Args)
	}
}

// PGArrayLiteral is the PostgreSQL form of the same payload: bare elements
// unquoted, strings with spaces or quotes double-quoted and escaped, NULL for
// JSON null.
func TestPGArrayLiteral(t *testing.T) {
	cases := []struct {
		in   []any
		want string
	}{
		{[]any{"go", "sql"}, `{go,sql}`},
		{[]any{json.Number("1"), json.Number("2.5")}, `{1,2.5}`},
		{[]any{"two words", `qu"ote`, nil}, `{"two words","qu\"ote",NULL}`},
		{[]any{true, false}, `{t,f}`},
		{[]any{}, `{}`},
	}
	for _, c := range cases {
		if got := PGArrayLiteral(c.in); got != c.want {
			t.Errorf("PGArrayLiteral(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
