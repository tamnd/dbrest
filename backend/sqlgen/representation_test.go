package sqlgen

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// colorRep mirrors the live _p11dr fixture: a "color" domain over integer with a
// cast function per direction (to-json formats, from-text parses a filter literal,
// from-json parses a write value), all in schema _p11dr.
var colorRep = ir.Rep{
	ToJSONSchema: "_p11dr", ToJSONFunc: "json",
	FromTextSchema: "_p11dr", FromTextFunc: "color",
	FromJSONSchema: "_p11dr", FromJSONFunc: "color",
}

func TestRepReadAppliesToJSON(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Select:   []ir.SelectItem{col("id"), col("c")},
		Reps:     map[string]ir.Rep{"c": colorRep},
	})
	want := `SELECT "id", "_p11dr"."json"("c") AS "c" FROM "shirts"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestRepFilterAppliesFromText(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"c"}, Op: ir.OpEq, Value: ir.Value{Text: "#ff0000"}})
	st, err := CompileRead(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Where:    &where,
		Reps:     map[string]ir.Rep{"c": colorRep},
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	want := `SELECT * FROM "shirts" WHERE "c" = "_p11dr"."color"($1::text)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 1 || st.Args[0] != "#ff0000" {
		t.Errorf("Args = %#v, want [#ff0000]", st.Args)
	}
}

func TestRepOrderingFilterAppliesFromText(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"c"}, Op: ir.OpGte, Value: ir.Value{Text: "#000080"}})
	st, err := CompileRead(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Where:    &where,
		Reps:     map[string]ir.Rep{"c": colorRep},
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	want := `SELECT * FROM "shirts" WHERE "c" >= "_p11dr"."color"($1::text)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

func TestRepInsertAppliesFromJSON(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Write: &ir.WriteSpec{
			Columns: []string{"c"},
			Rows:    []map[string]ir.Value{{"c": ir.Value{JSON: "#0000ff"}}},
		},
		Reps: map[string]ir.Rep{"c": colorRep},
	}, nil)
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "shirts" ("c") VALUES ("_p11dr"."color"($1::json))`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 1 || st.Args[0] != `"#0000ff"` {
		t.Errorf("Args = %#v, want [\"#0000ff\"]", st.Args)
	}
}

func TestRepUpdateAppliesFromJSON(t *testing.T) {
	st, err := CompileUpdate(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Write: &ir.WriteSpec{
			Set: map[string]ir.Value{"c": {JSON: "#00ff00"}},
		},
		Reps: map[string]ir.Rep{"c": colorRep},
	}, nil)
	if err != nil {
		t.Fatalf("CompileUpdate: %v", err)
	}
	want := `UPDATE "shirts" SET "c" = "_p11dr"."color"($1::json)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
	if len(st.Args) != 1 || st.Args[0] != `"#00ff00"` {
		t.Errorf("Args = %#v, want [\"#00ff00\"]", st.Args)
	}
}

func TestRepInsertReturningAppliesToJSON(t *testing.T) {
	st, err := CompileInsert(stub{}, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Write: &ir.WriteSpec{
			Columns: []string{"id", "c"},
			Rows:    []map[string]ir.Value{{"id": jnum("1"), "c": ir.Value{JSON: "#0000ff"}}},
			Return:  ir.ReturnRepresentation,
		},
		Reps: map[string]ir.Rep{"c": colorRep},
	}, []string{"id", "c"})
	if err != nil {
		t.Fatalf("CompileInsert: %v", err)
	}
	want := `INSERT INTO "shirts" ("id", "c") VALUES ($1, "_p11dr"."color"($2::json)) ` +
		`RETURNING "id", "_p11dr"."json"("c") AS "c"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// repWhere compiles a single-filter read over the shirts fixture and returns the
// statement, keeping the operator tests short.
func repWhere(t *testing.T, d Dialect, c ir.Compare) *Statement {
	t.Helper()
	where := ir.Cond(c)
	st, err := CompileRead(d, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Where:    &where,
		Reps:     map[string]ir.Rep{"c": colorRep},
	})
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	return st
}

// TestRepFilterMatchAppliesFromText: a regex match on a represented column parses
// the pattern through the from-text cast, matching PostgREST's match/imatch path.
func TestRepFilterMatchAppliesFromText(t *testing.T) {
	st := repWhere(t, stub{}, ir.Compare{Path: []string{"c"}, Op: ir.OpMatch, Value: ir.Value{Text: "#ff"}})
	want := `SELECT * FROM "shirts" WHERE "c" ~ "_p11dr"."color"($1::text)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestRepFilterContainsAppliesFromText: the simple-operator path (cs/cd/ov, range)
// parses the operand through the from-text cast, as PostgREST does.
func TestRepFilterContainsAppliesFromText(t *testing.T) {
	st := repWhere(t, stub{}, ir.Compare{Path: []string{"c"}, Op: ir.OpContains, Value: ir.Value{Text: "{1,2}"}})
	want := `SELECT * FROM "shirts" WHERE "c" @> "_p11dr"."color"($1::text)`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestRepFilterLikeStaysRaw: like/ilike carry a wildcard pattern, so PostgREST
// binds them raw even on a represented column. Confirm dbrest does not wrap them.
func TestRepFilterLikeStaysRaw(t *testing.T) {
	st := repWhere(t, stub{}, ir.Compare{Path: []string{"c"}, Op: ir.OpLike, Value: ir.Value{Text: "#ff%"}})
	want := `SELECT * FROM "shirts" WHERE "c" LIKE $1`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestRepFilterInPerElement: on an engine without a native = ANY list bind, IN
// expands per element and each element parses through the from-text cast.
func TestRepFilterInPerElement(t *testing.T) {
	st := repWhere(t, stub{}, ir.Compare{Path: []string{"c"}, Op: ir.OpIn, Value: ir.Value{List: []string{"#0000ff", "#00ff00"}}})
	want := `SELECT * FROM "shirts" WHERE "c" IN (` +
		`"_p11dr"."color"($1::text), "_p11dr"."color"($2::text))`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// anyListStub is a stub whose InList binds the list as one = ANY array, like the
// PostgreSQL dialect, so the IN representation path can be asserted in its native
// unnest form.
type anyListStub struct{ stub }

func (anyListStub) InList(col string) (string, bool) {
	return col + " = ANY(" + PatternMark + ")", true
}

// TestRepFilterInAnyUnnest: on a = ANY engine the represented IN list parses each
// element over the unpacked array, matching PostgREST's pgFmtArrayLiteralForField.
func TestRepFilterInAnyUnnest(t *testing.T) {
	st := repWhere(t, anyListStub{}, ir.Compare{Path: []string{"c"}, Op: ir.OpIn, Value: ir.Value{List: []string{"#0000ff", "#00ff00"}}})
	want := `SELECT * FROM "shirts" WHERE "c" = ANY(` +
		`(SELECT "_p11dr"."color"(unnest($1::text[]))))`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// TestRepReadExplicitCastOptsOut confirms an explicit client cast (col::type)
// suppresses the to-json representation: the client asked for a specific
// rendering, so the domain's formatter is not applied.
func TestRepReadExplicitCastOptsOut(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "shirts"},
		Select:   []ir.SelectItem{col("id"), ir.Column{Path: []string{"c"}, Cast: "text"}},
		Reps:     map[string]ir.Rep{"c": colorRep},
	})
	want := `SELECT "id", CAST("c" AS text) AS "c" FROM "shirts"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}
