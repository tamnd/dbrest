package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// A computed field in the select list renders as schema.func(row), where the row
// is the bare relation name at the top level, and is aliased to the field name
// only when the client renamed it.
func TestComputedFieldSelect(t *testing.T) {
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "authors"},
		Select:   []ir.SelectItem{col("id"), col("full_name")},
		Computed: map[string]string{"full_name": "public"},
	}
	st := compile(t, q)
	want := `SELECT "id", "public"."full_name"("authors") FROM "public"."authors"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// A renamed computed field carries an explicit alias so the output key is the one
// the client asked for, not the function name.
func TestComputedFieldAliased(t *testing.T) {
	q := &ir.Query{
		Relation: ir.Ref{Name: "authors"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"full_name"}, Alias: "name"}},
		Computed: map[string]string{"full_name": "public"},
	}
	st := compile(t, q)
	want := `SELECT "public"."full_name"("authors") AS "name" FROM "authors"`
	if st.SQL != want {
		t.Errorf("SQL = %q, want %q", st.SQL, want)
	}
}

// A computed field is filterable: a predicate on it lowers to the function call,
// not a bare column, so the WHERE references schema.func(row).
func TestComputedFieldFilter(t *testing.T) {
	var where ir.Cond = ir.Compare{
		Path: []string{"full_name"}, Op: ir.OpEq, Value: ir.Value{Text: "Ada Lovelace"},
	}
	q := &ir.Query{
		Relation: ir.Ref{Name: "authors"},
		Select:   []ir.SelectItem{col("id")},
		Where:    &where,
		Computed: map[string]string{"full_name": "public"},
	}
	st := compile(t, q)
	if !strings.Contains(st.SQL, `"public"."full_name"("authors") = `) {
		t.Errorf("filter did not render computed call: %q", st.SQL)
	}
}

// A computed field is orderable: ORDER BY references the function call.
func TestComputedFieldOrder(t *testing.T) {
	q := &ir.Query{
		Relation: ir.Ref{Name: "authors"},
		Select:   []ir.SelectItem{col("id")},
		Order:    []ir.OrderTerm{{Path: []string{"full_name"}}},
		Computed: map[string]string{"full_name": "public"},
	}
	st := compile(t, q)
	if !strings.Contains(st.SQL, `ORDER BY "public"."full_name"("authors")`) {
		t.Errorf("order did not render computed call: %q", st.SQL)
	}
}
