package schema

import "testing"

// viewModel wires a films table with a foreign key to directors, plus a view
// film_view that projects the film columns (including director_id) one-to-one.
// The view should inherit the films->directors foreign key under its own columns.
func viewModel() *Model {
	directors := &Relation{Schema: "public", Name: "directors", Columns: cols("id", "name")}
	films := &Relation{
		Schema:     "public",
		Name:       "films",
		Columns:    cols("id", "title", "director_id"),
		PrimaryKey: []string{"id"},
		ForeignKeys: []*ForeignKey{{
			Name: "films_director_id_fkey", Columns: []string{"director_id"},
			RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
		}},
	}
	filmView := &Relation{
		Schema:  "public",
		Name:    "film_view",
		Kind:    KindView,
		Columns: cols("id", "title", "director_id"),
		ViewColumns: []ViewColumn{
			{Name: "id", BaseSchema: "public", BaseRelation: "films", BaseColumn: "id"},
			{Name: "title", BaseSchema: "public", BaseRelation: "films", BaseColumn: "title"},
			{Name: "director_id", BaseSchema: "public", BaseRelation: "films", BaseColumn: "director_id"},
		},
	}
	return NewModel([]*Relation{directors, films, filmView})
}

// TestViewInheritsForwardForeignKey covers 01.11: a view that exposes the FK
// column embeds the referenced table as a to-one, the same as the base table.
func TestViewInheritsForwardForeignKey(t *testing.T) {
	m := viewModel()
	cands, found := m.Relationships(rel(t, m, "film_view"), "directors", []string{"public"})
	if !found {
		t.Fatal("directors not found")
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Card != CardToOne {
		t.Errorf("Card = %v, want to-one", cands[0].Card)
	}
	if cands[0].Local[0] != "director_id" {
		t.Errorf("Local = %v, want [director_id]", cands[0].Local)
	}
}

// TestViewEmbeddedFromBaseTable covers the reverse direction: a base table
// embeds the view as a to-many through the projected key's reverse view.
func TestViewEmbeddedFromBaseTable(t *testing.T) {
	m := viewModel()
	cands, _ := m.Relationships(rel(t, m, "directors"), "film_view", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Card != CardToMany {
		t.Errorf("Card = %v, want to-many", cands[0].Card)
	}
	if cands[0].Foreign[0] != "director_id" {
		t.Errorf("Foreign = %v, want [director_id]", cands[0].Foreign)
	}
}

// TestViewForeignKeyAcceptsBaseTableHint covers the third hint kind: a
// view-sourced relationship accepts the base table name as a disambiguation hint.
func TestViewForeignKeyAcceptsBaseTableHint(t *testing.T) {
	m := viewModel()
	cands, _ := m.Relationships(rel(t, m, "film_view"), "directors", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if !cands[0].MatchesHint("films") {
		t.Error("view relationship should accept the base table name films as a hint")
	}
}

// TestViewWithoutFKColumnInheritsNothing covers the PostgREST condition: a view
// that drops the foreign-key column does not inherit the relationship.
func TestViewWithoutFKColumnInheritsNothing(t *testing.T) {
	directors := &Relation{Schema: "public", Name: "directors", Columns: cols("id", "name")}
	films := &Relation{
		Schema:  "public",
		Name:    "films",
		Columns: cols("id", "title", "director_id"),
		ForeignKeys: []*ForeignKey{{
			Name: "films_director_id_fkey", Columns: []string{"director_id"},
			RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
		}},
	}
	// The view exposes only id and title; director_id does not survive.
	titlesView := &Relation{
		Schema:  "public",
		Name:    "titles",
		Kind:    KindView,
		Columns: cols("id", "title"),
		ViewColumns: []ViewColumn{
			{Name: "id", BaseSchema: "public", BaseRelation: "films", BaseColumn: "id"},
			{Name: "title", BaseSchema: "public", BaseRelation: "films", BaseColumn: "title"},
		},
	}
	m := NewModel([]*Relation{directors, films, titlesView})
	cands, _ := m.Relationships(rel(t, m, "titles"), "directors", []string{"public"})
	if len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0 (FK column dropped by the view)", len(cands))
	}
}

// TestViewOverViewChainsForeignKey covers recursive resolution: a view selecting
// from another view inherits the foreign key through the chain.
func TestViewOverViewChainsForeignKey(t *testing.T) {
	directors := &Relation{Schema: "public", Name: "directors", Columns: cols("id", "name")}
	films := &Relation{
		Schema:  "public",
		Name:    "films",
		Columns: cols("id", "title", "director_id"),
		ForeignKeys: []*ForeignKey{{
			Name: "films_director_id_fkey", Columns: []string{"director_id"},
			RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
		}},
	}
	inner := &Relation{
		Schema:  "public",
		Name:    "film_view",
		Kind:    KindView,
		Columns: cols("id", "title", "director_id"),
		ViewColumns: []ViewColumn{
			{Name: "id", BaseSchema: "public", BaseRelation: "films", BaseColumn: "id"},
			{Name: "title", BaseSchema: "public", BaseRelation: "films", BaseColumn: "title"},
			{Name: "director_id", BaseSchema: "public", BaseRelation: "films", BaseColumn: "director_id"},
		},
	}
	// outer selects from the inner view, renaming director_id to dir.
	outer := &Relation{
		Schema:  "public",
		Name:    "film_view2",
		Kind:    KindView,
		Columns: cols("id", "dir"),
		ViewColumns: []ViewColumn{
			{Name: "id", BaseSchema: "public", BaseRelation: "film_view", BaseColumn: "id"},
			{Name: "dir", BaseSchema: "public", BaseRelation: "film_view", BaseColumn: "director_id"},
		},
	}
	m := NewModel([]*Relation{directors, films, inner, outer})
	cands, _ := m.Relationships(rel(t, m, "film_view2"), "directors", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (FK chained through the inner view)", len(cands))
	}
	if cands[0].Local[0] != "dir" {
		t.Errorf("Local = %v, want [dir] (renamed by the outer view)", cands[0].Local)
	}
}
