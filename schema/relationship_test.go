package schema

import "testing"

// buildEmbedModel wires the canonical embedding fixture: films reference one
// director (forward FK, to-one), directors own many films (the reverse view,
// to-many), and films relate to actors through the roles junction (many-to-many).
func buildEmbedModel() *Model {
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
	actors := &Relation{Schema: "public", Name: "actors", Columns: cols("id", "name")}
	roles := &Relation{
		Schema:  "public",
		Name:    "roles",
		Columns: cols("film_id", "actor_id"),
		ForeignKeys: []*ForeignKey{
			{Name: "roles_film_id_fkey", Columns: []string{"film_id"}, RefSchema: "public", RefRelation: "films", RefColumns: []string{"id"}},
			{Name: "roles_actor_id_fkey", Columns: []string{"actor_id"}, RefSchema: "public", RefRelation: "actors", RefColumns: []string{"id"}},
		},
	}
	return NewModel([]*Relation{directors, films, actors, roles})
}

func cols(names ...string) []*Column {
	out := make([]*Column, len(names))
	for i, n := range names {
		out[i] = &Column{Name: n, Type: "text", Position: i + 1}
	}
	return out
}

func rel(t *testing.T, m *Model, name string) *Relation {
	t.Helper()
	r, ok := m.Lookup(name, []string{"public"})
	if !ok {
		t.Fatalf("relation %q not in model", name)
	}
	return r
}

func TestRelationshipsForwardToOne(t *testing.T) {
	m := buildEmbedModel()
	cands, found := m.Relationships(rel(t, m, "films"), "directors", []string{"public"})
	if !found {
		t.Fatal("directors not found")
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Card != CardToOne {
		t.Errorf("Card = %v, want to-one", c.Card)
	}
	if got := c.Local; len(got) != 1 || got[0] != "director_id" {
		t.Errorf("Local = %v, want [director_id]", got)
	}
	if got := c.Foreign; len(got) != 1 || got[0] != "id" {
		t.Errorf("Foreign = %v, want [id]", got)
	}
}

func TestRelationshipsBackwardToMany(t *testing.T) {
	m := buildEmbedModel()
	cands, _ := m.Relationships(rel(t, m, "directors"), "films", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Card != CardToMany {
		t.Errorf("Card = %v, want to-many", c.Card)
	}
	// The reverse view joins the director's id to the film's director_id.
	if c.Local[0] != "id" || c.Foreign[0] != "director_id" {
		t.Errorf("join = %v -> %v, want [id] -> [director_id]", c.Local, c.Foreign)
	}
}

func TestRelationshipsManyToManyJunction(t *testing.T) {
	m := buildEmbedModel()
	cands, _ := m.Relationships(rel(t, m, "films"), "actors", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Card != CardToMany {
		t.Errorf("Card = %v, want to-many", c.Card)
	}
	if c.Junction == nil || c.Junction.Name != "roles" {
		t.Fatalf("Junction = %v, want roles", c.Junction)
	}
	if c.Local[0] != "id" || c.JLocal[0] != "film_id" {
		t.Errorf("parent hop = %v = %v, want id = film_id", c.Local, c.JLocal)
	}
	if c.JForeign[0] != "actor_id" || c.Foreign[0] != "id" {
		t.Errorf("target hop = %v = %v, want actor_id = id", c.JForeign, c.Foreign)
	}
}

func TestRelationshipsTargetMissing(t *testing.T) {
	m := buildEmbedModel()
	_, found := m.Relationships(rel(t, m, "films"), "nope", []string{"public"})
	if found {
		t.Error("found should be false for an unknown target")
	}
}

func TestRelationshipsNoEdge(t *testing.T) {
	m := buildEmbedModel()
	// directors and actors share no foreign key and no junction between them.
	cands, found := m.Relationships(rel(t, m, "directors"), "actors", []string{"public"})
	if !found {
		t.Fatal("actors exists, found should be true")
	}
	if len(cands) != 0 {
		t.Errorf("got %d candidates, want 0", len(cands))
	}
}

func TestRelationshipsAmbiguous(t *testing.T) {
	// Two foreign keys from films to people (director and writer) make an
	// unqualified embed of people ambiguous.
	people := &Relation{Schema: "public", Name: "people", Columns: cols("id", "name")}
	films := &Relation{
		Schema: "public", Name: "films", Columns: cols("id", "director_id", "writer_id"),
		ForeignKeys: []*ForeignKey{
			{Name: "films_director_id_fkey", Columns: []string{"director_id"}, RefSchema: "public", RefRelation: "people", RefColumns: []string{"id"}},
			{Name: "films_writer_id_fkey", Columns: []string{"writer_id"}, RefSchema: "public", RefRelation: "people", RefColumns: []string{"id"}},
		},
	}
	m := NewModel([]*Relation{people, films})
	cands, _ := m.Relationships(rel(t, m, "films"), "people", []string{"public"})
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2", len(cands))
	}
	// A hint on the writer column selects exactly one.
	matched := 0
	for _, c := range cands {
		if c.MatchesHint("writer_id") {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("writer_id hint matched %d edges, want 1", matched)
	}
}
