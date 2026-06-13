package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/schema"
)

// computedRelModel wires authors with two computed relationships to books: a
// set-returning one (to-many) and a single-row one (to-one). The edges carry the
// function to call, not join columns.
func computedRelModel() *schema.Model {
	cols := func(names ...string) []*schema.Column {
		out := make([]*schema.Column, len(names))
		for i, n := range names {
			out[i] = &schema.Column{Name: n, Type: "text", Position: i + 1}
		}
		return out
	}
	books := &schema.Relation{Schema: "public", Name: "books", Columns: cols("id", "title")}
	authors := &schema.Relation{
		Schema:  "public",
		Name:    "authors",
		Columns: cols("id", "name"),
		ComputedRels: []schema.ComputedRel{
			{Name: "books", FuncSchema: "public", TargetSchema: "public", TargetName: "books", Card: schema.CardToMany},
			{Name: "first_book", FuncSchema: "public", TargetSchema: "public", TargetName: "books", Card: schema.CardToOne},
		},
	}
	return schema.NewModel([]*schema.Relation{authors, books})
}

// A to-many computed relationship embeds by calling the function on the parent
// row in the subquery FROM, with no join predicate (the row argument correlates).
func TestComputedRelToMany(t *testing.T) {
	m := computedRelModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "authors"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"name"}}, ir.EmbedRef{Index: 0}},
		Embeds: []ir.Embed{{
			OutKey: "books",
			Target: ir.Ref{Schema: "public", Name: "books"},
			Rel:    relateNamed(t, m, "authors", "books", "books"),
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"title"}}}},
		}},
	}
	got := compileEmbed(t, q).SQL
	if !strings.Contains(got, `FROM "public"."books"(t0) t1 WHERE TRUE`) {
		t.Errorf("to-many computed-rel embed did not render function call:\n%s", got)
	}
}

// A to-one computed relationship renders a single-object subquery over the
// function call, again correlating through the row argument.
func TestComputedRelToOne(t *testing.T) {
	m := computedRelModel()
	rel := relateNamed(t, m, "authors", "books", "first_book")
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "authors"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"name"}}, ir.EmbedRef{Index: 0}},
		Embeds: []ir.Embed{{
			OutKey: "first_book",
			Target: ir.Ref{Schema: "public", Name: "books"},
			Rel:    rel,
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"title"}}}},
		}},
	}
	got := compileEmbed(t, q).SQL
	want := `(SELECT json_object('title', t1."title") FROM "public"."first_book"(t0) t1 WHERE TRUE LIMIT 1)`
	if !strings.Contains(got, want) {
		t.Errorf("to-one computed-rel embed:\n got %s\nwant substring %s", got, want)
	}
}

// relateNamed picks the single edge from parent to target whose name matches,
// when more than one edge connects them (two computed relationships here).
func relateNamed(t *testing.T, m *schema.Model, parent, target, name string) *schema.Relationship {
	t.Helper()
	p, ok := m.Lookup(parent, []string{"public"})
	if !ok {
		t.Fatalf("parent %q not in model", parent)
	}
	cands, found := m.Relationships(p, target, []string{"public"})
	if !found {
		t.Fatalf("relateNamed(%s,%s): target not found", parent, target)
	}
	for i := range cands {
		if cands[i].Name == name {
			return &cands[i]
		}
	}
	t.Fatalf("relateNamed(%s,%s,%s): no edge named %q among %d", parent, target, name, name, len(cands))
	return nil
}

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
