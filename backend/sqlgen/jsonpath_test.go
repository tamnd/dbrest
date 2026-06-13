package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// 07.1: a JSON-path projection lowers through the dialect's JSONPath. The stub
// spells the PostgreSQL native chain, with a digit hop as an array index and a
// final ->> producing text.
func TestCompileJSONPathProjection(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"data", "phones", "0", "number"}, Last: ir.JSONArrow2},
		},
	})
	if !strings.Contains(st.SQL, `"data"->'phones'->0->>'number'`) {
		t.Errorf("SQL = %q, want the native ->/->> chain", st.SQL)
	}
	// The output field is named for the last hop.
	if !strings.Contains(st.SQL, `AS "number"`) {
		t.Errorf("SQL = %q, want the projection aliased to the last hop", st.SQL)
	}
}

// A JSON-path filter lowers the same way; a final -> keeps the json typing.
func TestCompileJSONPathFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path: []string{"data", "blood_type"}, Last: ir.JSONArrow2,
		Op: ir.OpEq, Value: ir.Value{Text: "A-"},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if !strings.Contains(st.SQL, `"data"->>'blood_type' = `) {
		t.Errorf("SQL = %q, want a ->> text predicate", st.SQL)
	}
	if len(st.Args) != 1 || st.Args[0] != "A-" {
		t.Errorf("Args = %v, want the value bound", st.Args)
	}
}

// eq.true against a JSON ->> extract binds the literal word as text, never the
// boolean TRUE: a JSON field holding "true" must match (07.4 coercion is
// column-type driven and a JSON access is not a boolean column).
func TestCompileJSONPathEqTrueBindsText(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path: []string{"data", "flag"}, Last: ir.JSONArrow2,
		Op: ir.OpEq, Value: ir.Value{Text: "true"},
	})
	st := compile(t, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if strings.Contains(st.SQL, "TRUE") {
		t.Errorf("SQL = %q, want the word bound, not the boolean TRUE", st.SQL)
	}
	if len(st.Args) != 1 || st.Args[0] != "true" {
		t.Errorf("Args = %v, want [true] bound as text", st.Args)
	}
}

// Ordering by a JSON path lowers through the dialect in the ORDER BY.
func TestCompileJSONPathOrder(t *testing.T) {
	st := compile(t, &ir.Query{
		Relation: ir.Ref{Name: "t"},
		Order:    []ir.OrderTerm{{Path: []string{"data", "created_at"}, Last: ir.JSONArrow2, Desc: true}},
	})
	if !strings.Contains(st.SQL, `ORDER BY "data"->>'created_at' DESC`) {
		t.Errorf("SQL = %q, want ORDER BY on the ->> extract", st.SQL)
	}
}

// An engine without JSON paths reports ok=false and the request is PGRST127
// rather than emitting wrong SQL.
type noJSONDialect struct{ stub }

func (noJSONDialect) JSONPath(string, []string, bool) (string, bool) { return "", false }

func TestCompileJSONPathCapabilityGap(t *testing.T) {
	where := ir.Cond(ir.Compare{
		Path: []string{"data", "x"}, Last: ir.JSONArrow2, Op: ir.OpEq, Value: ir.Value{Text: "1"},
	})
	_, err := CompileRead(noJSONDialect{}, &ir.Query{Relation: ir.Ref{Name: "t"}, Where: &where})
	if err == nil {
		t.Fatal("expected an unsupported error for a JSON path on an engine without one")
	}
	if err.Code != "PGRST127" {
		t.Errorf("code = %s, want PGRST127", err.Code)
	}
}
